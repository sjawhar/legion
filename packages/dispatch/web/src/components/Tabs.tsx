import { type KeyboardEvent, type ReactNode, useRef } from "react";

import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  textSecondaryOnCanvas,
} from "../theme/classes";

export interface TabDefinition<Id extends string> {
  id: Id;
  label: string;
}

export function Tabs<Id extends string>({
  activeTab,
  ariaLabel,
  idPrefix,
  onBeforeTabChange,
  onSelect,
  tabs,
}: {
  activeTab: Id;
  ariaLabel: string;
  idPrefix: string;
  onBeforeTabChange?: (current: Id) => void;
  onSelect: (tab: Id) => void;
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
    tabRefs.current[next.id]?.focus();
  };

  return (
    <div
      aria-label={ariaLabel}
      className={`mb-3 flex gap-1 border-b ${borderDefault}`}
      role="tablist"
    >
      {tabs.map((tab) => (
        <button
          aria-controls={`${idPrefix}-${tab.id}-panel`}
          aria-selected={activeTab === tab.id}
          className={
            activeTab === tab.id
              ? `min-h-11 border-b-2 px-3 py-2 text-sm font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
              : `min-h-11 px-3 py-2 text-sm ${textSecondaryOnCanvas}`
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
        </button>
      ))}
    </div>
  );
}
