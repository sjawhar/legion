import { type KeyboardEvent, type ReactNode, useEffect, useRef } from "react";

import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { Pill } from "./Pill";

export interface TabDefinition<Id extends string> {
  id: Id;
  label: string;
  /** Shown as a small pill after the label; the tab's accessible name is `<label> (<count>)`. */
  count?: number;
}

/** A tablist may be narrower than its tabs (a phone); a tab focused from the keyboard or
 *  selected must be fully visible, which Chromium's `focus()` alone does not guarantee. Only the
 *  tablist scrolls: `scrollIntoView` would also move the page, undoing the per-tab scroll
 *  position IssuePage restores when a panel comes back. */
function revealTab(node: HTMLElement | null | undefined): void {
  const list = node?.parentElement;
  if (node === null || node === undefined || list === null || list === undefined) {
    return;
  }
  const listBox = list.getBoundingClientRect();
  const box = node.getBoundingClientRect();
  if (box.left < listBox.left) {
    list.scrollLeft += box.left - listBox.left;
  } else if (box.right > listBox.right) {
    list.scrollLeft += box.right - listBox.right;
  }
}

export function Tabs<Id extends string>({
  activeTab,
  ariaLabel,
  compact = false,
  idPrefix,
  onBeforeTabChange,
  onSelect,
  tabs,
  trailing,
}: {
  activeTab: Id;
  ariaLabel: string;
  compact?: boolean;
  idPrefix: string;
  onBeforeTabChange?: (current: Id) => void;
  onSelect: (tab: Id) => void;
  trailing?: ReactNode;
  tabs: readonly TabDefinition<Id>[];
}): ReactNode {
  const tabRefs = useRef<Partial<Record<Id, HTMLButtonElement | null>>>({});
  const pointerCapturedScroll = useRef(false);

  const selectTab = (tab: Id) => {
    if (!pointerCapturedScroll.current) {
      onBeforeTabChange?.(activeTab);
    }
    pointerCapturedScroll.current = false;
    onSelect(tab);
  };
  const onTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, current: Id) => {
    const index = tabs.findIndex((tab) => tab.id === current);
    let nextIndex: number | undefined;
    if (event.key === "ArrowRight") {
      nextIndex = (index + 1) % tabs.length;
    } else if (event.key === "ArrowLeft") {
      nextIndex = (index - 1 + tabs.length) % tabs.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = tabs.length - 1;
    }
    if (nextIndex === undefined) {
      return;
    }
    event.preventDefault();
    const next = tabs[nextIndex];
    if (next === undefined) {
      return;
    }
    selectTab(next.id);
    const node = tabRefs.current[next.id];
    node?.focus();
    revealTab(node);
  };

  useEffect(() => {
    revealTab(tabRefs.current[activeTab]);
  }, [activeTab]);

  const tabMinHeight = compact ? "min-h-11 md:min-h-9" : "min-h-11";
  // Compact tabs sit tighter on phones so the issue's four tabs, one carrying a count pill, fit a
  // 390 px viewport on one line; narrower still (360 px Android), the tablist scrolls sideways
  // and `revealTab` keeps the focused or selected tab fully visible.
  const tabPadding = compact ? "px-1.5 md:px-2" : "px-3";
  const tabGap = compact ? "gap-0 md:gap-1" : "gap-1";

  return (
    <div
      className={`${compact ? "mb-0" : "mb-3"} flex flex-wrap items-center gap-1 border-b ${borderDefault}`}
    >
      <div
        aria-label={ariaLabel}
        className={`flex min-w-0 ${tabGap} overflow-x-auto [scrollbar-width:thin]`}
        role="tablist"
      >
        {tabs.map((tab) => (
          <button
            aria-controls={`${idPrefix}-${tab.id}-panel`}
            aria-label={tab.count === undefined ? undefined : `${tab.label} (${tab.count})`}
            aria-selected={activeTab === tab.id}
            className={
              activeTab === tab.id
                ? `${tabMinHeight} ${tabPadding} inline-flex shrink-0 items-center gap-1.5 border-b-2 py-2 text-sm font-semibold whitespace-nowrap ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                : `${tabMinHeight} ${tabPadding} inline-flex shrink-0 items-center gap-1.5 py-2 text-sm whitespace-nowrap ${textSecondaryOnCanvas}`
            }
            id={`${idPrefix}-${tab.id}-tab`}
            key={tab.id}
            onClick={() => selectTab(tab.id)}
            onKeyDown={(event) => onTabKeyDown(event, tab.id)}
            onPointerDown={() => {
              onBeforeTabChange?.(activeTab);
              pointerCapturedScroll.current = true;
            }}
            ref={(node) => {
              tabRefs.current[tab.id] = node;
            }}
            role="tab"
            tabIndex={activeTab === tab.id ? 0 : -1}
            type="button"
          >
            {tab.label}
            {tab.count === undefined ? null : <Pill>{tab.count}</Pill>}
          </button>
        ))}
      </div>
      {trailing}
    </div>
  );
}
