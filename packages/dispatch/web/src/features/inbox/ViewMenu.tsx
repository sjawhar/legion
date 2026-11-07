import { type ReactNode, useEffect, useId, useRef } from "react";

import {
  borderDefault,
  card,
  focusVisibleRing,
  selectedCardBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  INBOX_GROUP_BYS,
  INBOX_SHOWS,
  INBOX_SORTS,
  type InboxGroupBy,
  type InboxShow,
  type InboxSort,
} from "../refs/routes";
import { THREAD_ORDER_LABELS, THREAD_ORDERS, type ThreadOrder } from "../threads/thread-order";
import { DIMENSION_LABELS } from "./grouping";

/** How the list may be ordered, as the menu names each choice. */
const SORT_LABELS: Record<InboxSort, string> = { ...DIMENSION_LABELS, newest: "Newest first" };

/** How the list may be banded, as the menu names each choice. */
const GROUP_LABELS: Record<InboxGroupBy, string> = {
  ...DIMENSION_LABELS,
  none: "None",
  "waiting-on": "Waiting on",
};

/** Which list is showing, as the menu names each choice. */
const SHOW_LABELS: Record<InboxShow, string> = { dismissed: "Dismissed", inbox: "Inbox" };

/** One of a menu section's mutually exclusive choices. */
function Choice<T extends string>({
  chosen,
  group,
  label,
  onChoose,
  value,
}: {
  chosen: boolean;
  /** The radio group this choice belongs to, so the browser reads the section as one pick. */
  group: string;
  label: string;
  onChoose: (value: T) => void;
  value: T;
}): ReactNode {
  return (
    <label
      className={`flex min-h-11 w-full cursor-pointer items-center gap-2 rounded px-2 text-left text-sm ${
        chosen ? `font-semibold ${selectedCardBg} ${textSecondaryOnSurface}` : textSecondaryOnCanvas
      }`}
    >
      <input
        checked={chosen}
        className={`size-4 shrink-0 outline-none focus-visible:ring-2 ${focusVisibleRing}`}
        data-inbox-view-choice={value}
        name={group}
        onChange={() => onChoose(value)}
        type="radio"
        value={value}
      />
      {label}
    </label>
  );
}

function Section({ children, label }: { children: ReactNode; label: string }): ReactNode {
  return (
    <fieldset className="py-1">
      <legend
        className={`px-2 pb-1 text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}
      >
        {label}
      </legend>
      {children}
    </fieldset>
  );
}

/** What the menu shows and changes. The bar owns `open` and `onOpenChange`; the others are the
 *  Inbox's, which the bar only passes on (`InboxBar`'s `view` prop). */
export interface ViewMenuProps {
  group: InboxGroupBy;
  onGroupChange: (next: InboxGroupBy) => void;
  onOpenChange: (open: boolean) => void;
  onShowChange: (next: InboxShow) => void;
  onSortChange: (next: InboxSort) => void;
  onThreadOrderChange: (next: ThreadOrder) => void;
  open: boolean;
  /** Which of the reader's two lists is showing: their Inbox, or what they have dismissed. */
  show: InboxShow;
  sort: InboxSort;
  threadOrder: ThreadOrder;
}

/**
 * The Inbox bar's **View** menu: how the list is ordered, how it is banded, which of the reader's
 * two lists is showing - their Inbox, or what they have dismissed out of it - and which way a
 * thread's own replies read.
 *
 * The first three are the list's, so they ride in the address and a link carries them; the
 * thread order is the reader's own and is kept for their browser (`threads/thread-order.ts`).
 */
export function ViewMenu({
  group,
  onGroupChange,
  onOpenChange,
  onShowChange,
  onSortChange,
  onThreadOrderChange,
  open,
  show,
  sort,
  threadOrder,
}: ViewMenuProps): ReactNode {
  const id = useId();
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(false);

  useEffect(() => {
    if (!open && wasOpen.current) triggerRef.current?.focus();
    wasOpen.current = open;
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      onOpenChange(false);
    };
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
  }, [onOpenChange, open]);

  return (
    <div className="relative">
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
        data-inbox-view=""
        onClick={() => onOpenChange(!open)}
        ref={triggerRef}
        type="button"
      >
        View
      </button>
      {open ? (
        <div
          aria-label="View"
          className={`absolute right-0 z-50 mt-1 max-h-96 w-64 overflow-auto rounded-lg border p-1 shadow-lg ${card} ${borderDefault}`}
          onKeyDown={(event) => {
            if (event.key !== "Escape") return;
            event.preventDefault();
            event.stopPropagation();
            onOpenChange(false);
          }}
          ref={panelRef}
          role="menu"
        >
          <Section label="Sort by">
            {INBOX_SORTS.map((value) => (
              <Choice
                chosen={value === sort}
                group={`${id}-sort`}
                key={value}
                label={SORT_LABELS[value]}
                onChoose={onSortChange}
                value={value}
              />
            ))}
          </Section>
          <Section label="Group by">
            {INBOX_GROUP_BYS.map((value) => (
              <Choice
                chosen={value === group}
                group={`${id}-group`}
                key={value}
                label={GROUP_LABELS[value]}
                onChoose={onGroupChange}
                value={value}
              />
            ))}
          </Section>
          <Section label="Show">
            {INBOX_SHOWS.map((value) => (
              <Choice
                chosen={value === show}
                group={`${id}-show`}
                key={value}
                label={SHOW_LABELS[value]}
                onChoose={onShowChange}
                value={value}
              />
            ))}
          </Section>
          <Section label="Thread order">
            {THREAD_ORDERS.map((value) => (
              <Choice
                chosen={value === threadOrder}
                group={`${id}-order`}
                key={value}
                label={THREAD_ORDER_LABELS[value]}
                onChoose={onThreadOrderChange}
                value={value}
              />
            ))}
          </Section>
        </div>
      ) : null}
    </div>
  );
}
