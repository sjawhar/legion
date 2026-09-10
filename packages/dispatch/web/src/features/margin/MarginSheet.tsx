import { type ReactNode, useEffect, useRef, useState } from "react";

import type { Artifact, Comment, Event } from "../../api/types";
import { CommentsTab, type MarginComposer } from "./CommentsTab";
import type { ComposerAnchor, ComposerKind } from "./Composer";
import type { MarginSelection } from "./Margin";
import { PinnedTab } from "./PinnedTab";
import { SelectionMenu } from "./SelectionMenu";
import type { MarginItem, MarginItemAction, MarginTab } from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

interface MarginSheetProps {
  ArtifactsTabSlot?: () => ReactNode;
  asksPending: boolean;
  commentsPending: boolean;
  composer: MarginComposer | undefined;
  hoveredItemId: string | undefined;
  isClosed: boolean;
  issueKey: string | undefined;
  issuePending: boolean;
  items: MarginItem[];
  onAction: (id: string, action: MarginItemAction) => void;
  onCloseComposer: () => void;
  onReply: (comment: Comment) => void;
  onSelectionAction: (kind: ComposerKind, anchor: ComposerAnchor) => void;
  openAskCount: number;
  pinnedIds: string[];
  pinned: Event[];
  routeArtifactSlug: string | undefined;
  routeItemId: string | undefined;
  selectItem: (id: string) => void;
  selectedItemId: string | undefined;
  selection: MarginSelection | undefined;
  setHoveredItemId: (id: string | undefined) => void;
  setTab: (tab: MarginTab) => void;
  tab: MarginTab;
  visibleArtifact: Artifact | undefined;
}

export function MarginSheet({
  ArtifactsTabSlot,
  asksPending,
  commentsPending,
  composer,
  hoveredItemId,
  isClosed,
  issueKey,
  issuePending,
  items,
  onAction,
  onCloseComposer,
  onReply,
  onSelectionAction,
  openAskCount,
  pinned,
  pinnedIds,
  routeArtifactSlug,
  routeItemId,
  selectItem,
  selectedItemId,
  selection,
  setHoveredItemId,
  setTab,
  tab,
  visibleArtifact,
}: MarginSheetProps): ReactNode {
  const list = useRef<HTMLDivElement>(null);
  const [expandedIssueKey, setExpandedIssueKey] = useState<string>();
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const sheetExpanded = issueKey !== undefined && expandedIssueKey === issueKey;

  useEffect(() => {
    if (
      routeArtifactSlug === undefined ||
      visibleArtifact === undefined ||
      visibleArtifact.kind === "doc"
    ) {
      return;
    }
    setTab("artifacts");
    if (window.matchMedia("(max-width: 767px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeArtifactSlug, setTab, visibleArtifact]);

  useMarginListeners({
    items,
    list,
    routeItemId,
    selectItem,
    setHoveredItemId,
    setTab,
    sheetExpanded,
    tab,
    visibleArtifact,
  });

  return (
    <aside
      className={`fixed inset-x-0 bottom-0 z-10 border-t border-slate-200 bg-white shadow-[0_-8px_24px_rgba(15,23,42,0.08)] ${
        sheetExpanded ? "max-h-[85dvh] overflow-y-auto p-4" : "h-16 overflow-hidden"
      } md:static md:order-3 md:h-auto md:max-h-none md:w-96 md:overflow-visible md:border-t-0 md:border-l md:p-4 md:shadow-none`}
      data-expanded={sheetExpanded ? "true" : "false"}
      data-testid="margin-sheet"
    >
      <button
        aria-expanded={sheetExpanded}
        aria-label={sheetExpanded ? "Close review panel" : "Open review panel"}
        className="flex h-16 w-full items-center justify-between px-4 text-left text-sm font-semibold text-slate-800 md:hidden"
        onClick={() => {
          if (sheetDragMoved.current) {
            sheetDragMoved.current = false;
            return;
          }
          setExpandedIssueKey(sheetExpanded ? undefined : issueKey);
        }}
        onPointerDown={(event) => {
          sheetDragOrigin.current = event.clientY;
        }}
        onPointerUp={(event) => {
          const origin = sheetDragOrigin.current;
          sheetDragOrigin.current = undefined;
          if (origin === undefined || Math.abs(event.clientY - origin) < 12) {
            return;
          }
          sheetDragMoved.current = true;
          setExpandedIssueKey(event.clientY < origin ? issueKey : undefined);
        }}
        type="button"
      >
        <span>Review panel</span>
        <span className="font-normal text-slate-500">
          {openAskCount} open {openAskCount === 1 ? "ask" : "asks"}
        </span>
      </button>
      {selection === undefined || visibleArtifact === undefined || isClosed ? null : (
        <SelectionMenu
          onAction={(kind) => {
            // The composer lives in the sheet body, so acting on a selection opens it.
            setExpandedIssueKey(issueKey);
            onSelectionAction(kind, selection);
          }}
          selection={selection}
        />
      )}
      <div className={sheetExpanded ? "px-4 pb-4 md:px-0 md:pb-0" : "hidden md:block"}>
        <div className="flex border-b border-slate-200" role="tablist">
          {(["comments", "artifacts", "pinned"] as MarginTab[]).map((name) => (
            <button
              aria-selected={tab === name}
              className={
                tab === name
                  ? "border-b-2 border-sky-600 px-3 py-2 text-sm font-semibold text-sky-700"
                  : "px-3 py-2 text-sm text-slate-600"
              }
              key={name}
              onClick={() => setTab(name)}
              role="tab"
              type="button"
            >
              {name === "comments" ? "Comments" : name === "artifacts" ? "Artifacts" : "Pinned"}
            </button>
          ))}
        </div>
        {issueKey === undefined ? (
          <p className="pt-3 text-sm text-slate-500">Open an issue to review its margin.</p>
        ) : null}
        {issueKey !== undefined && visibleArtifact === undefined && issuePending ? (
          <p className="pt-3 text-sm text-slate-500">Loading margin…</p>
        ) : null}
        {tab === "artifacts" ? ArtifactsTabSlot === undefined ? null : <ArtifactsTabSlot /> : null}
        {tab === "pinned" ? <PinnedTab events={pinned} pinnedIds={pinnedIds} /> : null}
        {tab === "comments" && visibleArtifact !== undefined ? (
          <CommentsTab
            artifactSlug={visibleArtifact.slug}
            asksPending={asksPending}
            commentsPending={commentsPending}
            composer={composer}
            hoveredItemId={hoveredItemId}
            isClosed={isClosed}
            issueKey={issueKey ?? ""}
            items={items}
            list={list}
            onAction={onAction}
            onCloseComposer={onCloseComposer}
            onReply={onReply}
            selectedItemId={selectedItemId}
          />
        ) : null}
      </div>
    </aside>
  );
}
