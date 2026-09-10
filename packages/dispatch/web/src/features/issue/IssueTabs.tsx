import { type KeyboardEvent, type ReactNode, useRef } from "react";
import { useNavigate } from "react-router-dom";
import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath, type IssueTab } from "../refs/routes";

const tabs: { id: IssueTab; label: string }[] = [
  { id: "spec", label: "Spec" },
  { id: "log", label: "Log" },
  { id: "children", label: "Children" },
  { id: "artifacts", label: "Artifacts" },
];

/**
 * The Spec/Log/Children/Artifacts tablist. Follows the WAI-ARIA tabs pattern:
 * a labelled tablist, roving tabindex, and Left/Right/Home/End move and
 * activate the adjacent tab. Selecting a tab navigates to its route segment
 * (see `../refs/routes`), so the active tab is part of the URL rather than
 * local component state.
 */
export function IssueTabs({
  activeTab,
  issueKey,
  onBeforeTabChange,
}: {
  activeTab: IssueTab;
  issueKey: string;
  onBeforeTabChange: (current: IssueTab) => void;
}): ReactNode {
  const navigate = useNavigate();
  const tabRefs = useRef<Record<IssueTab, HTMLButtonElement | null>>({
    artifacts: null,
    children: null,
    log: null,
    spec: null,
  });
  const pointerCapturedScroll = useRef(false);

  const goToTab = (tab: IssueTab) => {
    if (!pointerCapturedScroll.current) {
      onBeforeTabChange(activeTab);
    }
    pointerCapturedScroll.current = false;
    navigate(buildIssuePath({ key: issueKey, kind: tab }));
  };
  const onTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, current: IssueTab) => {
    const order = tabs.map((t) => t.id);
    const index = order.indexOf(current);
    let nextIndex: number | undefined;
    if (event.key === "ArrowRight") {
      nextIndex = (index + 1) % order.length;
    } else if (event.key === "ArrowLeft") {
      nextIndex = (index - 1 + order.length) % order.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = order.length - 1;
    }
    if (nextIndex === undefined) {
      return;
    }
    event.preventDefault();
    const nextTab = order[nextIndex];
    if (nextTab === undefined) {
      return;
    }
    goToTab(nextTab);
    tabRefs.current[nextTab]?.focus();
  };

  return (
    <div
      aria-label="Issue detail"
      className={`mb-3 flex gap-1 border-b ${borderDefault}`}
      role="tablist"
    >
      {tabs.map((t) => (
        <button
          aria-controls={`issue-${t.id}-panel`}
          aria-selected={activeTab === t.id}
          className={
            activeTab === t.id
              ? `min-h-11 border-b-2 px-3 py-2 text-sm font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
              : `min-h-11 px-3 py-2 text-sm ${textSecondaryOnCanvas}`
          }
          id={`issue-${t.id}-tab`}
          key={t.id}
          onClick={() => goToTab(t.id)}
          onPointerDown={() => {
            onBeforeTabChange(activeTab);
            pointerCapturedScroll.current = true;
          }}
          onKeyDown={(event) => onTabKeyDown(event, t.id)}
          ref={(node) => {
            tabRefs.current[t.id] = node;
          }}
          role="tab"
          tabIndex={activeTab === t.id ? 0 : -1}
          type="button"
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}
