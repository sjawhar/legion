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
import { INBOX_GROUP_BYS, INBOX_SORTS, type InboxGroupBy, type InboxSort } from "../refs/routes";
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

/** What the menu shows and changes. The bar owns `open` and `onOpenChange`; the other ten are
 *  the Inbox's, which the bar only passes on (`InboxBar`'s `view` prop). */
export interface ViewMenuProps {
  /** How many thread rows the list would hold with the switch on, or undefined while the list
   *  is on Unread: that count is the read's own `total`, and the read the switch is off for
   *  counts the unread alone. It is read only once the reader has opened this menu, the one
   *  place it is drawn. */
  allThreadCount: number | undefined;
  group: InboxGroupBy;
  onGroupChange: (next: InboxGroupBy) => void;
  onOpenChange: (open: boolean) => void;
  onShowReadThreadsChange: (next: boolean) => void;
  onSortChange: (next: InboxSort) => void;
  onThreadOrderChange: (next: ThreadOrder) => void;
  open: boolean;
  showReadThreads: boolean;
  sort: InboxSort;
  threadOrder: ThreadOrder;
  /** How many thread rows the list holds with the switch off, its default. */
  unreadThreadCount: number;
}

/**
 * The Inbox bar's **View** menu: how the list is ordered, how it is banded, whether the threads
 * the reader has already read are in it, and which way a thread's own replies read.
 *
 * The first three are the list's, so they ride in the address and a link carries them; the
 * thread order is the reader's own and is kept for their browser (`threads/thread-order.ts`).
 */
export function ViewMenu({
  allThreadCount,
  group,
  onGroupChange,
  onOpenChange,
  onShowReadThreadsChange,
  onSortChange,
  onThreadOrderChange,
  open,
  showReadThreads,
  sort,
  threadOrder,
  unreadThreadCount,
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
          <Section label="Show read threads">
            <Choice
              chosen={!showReadThreads}
              group={`${id}-threads`}
              label={`Unread ${unreadThreadCount}`}
              onChoose={() => onShowReadThreadsChange(false)}
              value="unread"
            />
            <Choice
              chosen={showReadThreads}
              group={`${id}-threads`}
              label={allThreadCount === undefined ? "All threads" : `All threads ${allThreadCount}`}
              onChoose={() => onShowReadThreadsChange(true)}
              value="all"
            />
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
