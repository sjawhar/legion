import { type ReactNode, useRef } from "react";

import { useDialog, useMediaQuery } from "../shell/useDialog";
import { CommentsTab } from "./CommentsTab";
import type { MarginSheetModel } from "./Margin";
import { PinnedTab } from "./PinnedTab";
import { SelectionMenu } from "./SelectionMenu";
import type { MarginTab } from "./useMarginItems";

interface MarginSheetProps {
  ArtifactsTabSlot?: () => ReactNode;
  model: MarginSheetModel;
}

export function MarginSheet({ ArtifactsTabSlot, model }: MarginSheetProps): ReactNode {
  const {
    actions,
    composer,
    items: {
      asksPending,
      comments,
      commentsPending,
      commentListRef,
      isClosed,
      issueKey,
      issuePending,
      openAskCount,
      pinned,
      pinnedIds,
      visibleArtifact,
    },
    selection,
    sheet,
    tab,
  } = model;
  const selectionValue = selection.value;
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const isPhoneViewport = useMediaQuery("(max-width: 767px)");
  const dialog = useDialog<HTMLElement>({
    onClose: () => sheet.toggle(false),
    open: sheet.expanded && isPhoneViewport,
  });

  return (
    <>
      {sheet.expanded ? (
        <div
          aria-hidden="true"
          className="fixed inset-0 z-[9] bg-slate-950/50 md:hidden"
          onClick={() => sheet.toggle(false)}
        />
      ) : null}
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: aria-modal only applies while the expanded phone sheet makes this element role="dialog" */}
      <aside
        aria-label={sheet.expanded && isPhoneViewport ? "Review panel" : undefined}
        aria-modal={sheet.expanded && isPhoneViewport ? true : undefined}
        className={`fixed inset-x-0 bottom-0 z-10 border-t border-slate-200 bg-white shadow-[0_-8px_24px_rgba(15,23,42,0.08)] ${
          issueKey === undefined
            ? "hidden"
            : sheet.expanded
              ? "max-h-[85dvh] overflow-y-auto p-4"
              : "h-16 overflow-hidden"
        } md:static md:order-3 md:block md:h-auto md:max-h-none md:w-96 md:overflow-visible md:border-t-0 md:border-l md:p-4 md:shadow-none`}
        data-expanded={sheet.expanded ? "true" : "false"}
        data-testid="margin-sheet"
        ref={dialog.containerRef}
        role={sheet.expanded && isPhoneViewport ? "dialog" : undefined}
      >
        <div
          aria-hidden="true"
          className="mx-auto mb-1 h-1 w-10 rounded-full bg-slate-300 md:hidden"
        />
        <button
          aria-expanded={sheet.expanded}
          aria-label={sheet.expanded ? "Close review panel" : "Open review panel"}
          className="flex h-16 w-full items-center justify-between px-4 text-left text-sm font-semibold text-slate-800 md:hidden"
          onClick={() => {
            if (sheetDragMoved.current) {
              sheetDragMoved.current = false;
              return;
            }
            sheet.toggle();
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
            sheet.toggle(event.clientY < origin);
          }}
          type="button"
        >
          <span>Review panel</span>
          <span className="font-normal text-slate-500">
            {openAskCount} open {openAskCount === 1 ? "ask" : "asks"}
          </span>
        </button>
        {selectionValue === undefined || visibleArtifact === undefined || isClosed ? null : (
          <SelectionMenu
            onAction={(kind) => {
              // The composer lives in the sheet body, so acting on a selection opens it.
              sheet.toggle(true);
              actions.onSelectionAction(kind, selectionValue);
            }}
            selection={selectionValue}
          />
        )}
        <div className={sheet.expanded ? "px-4 pb-4 md:px-0 md:pb-0" : "hidden md:block"}>
          <div className="flex border-b border-slate-200" role="tablist">
            {(["comments", "artifacts", "pinned"] as MarginTab[]).map((name) => (
              <button
                aria-selected={tab.value === name}
                className={
                  tab.value === name
                    ? "border-b-2 border-sky-600 px-3 py-2 text-sm font-semibold text-sky-700"
                    : "px-3 py-2 text-sm text-slate-600"
                }
                key={name}
                onClick={() => tab.set(name)}
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
          {tab.value === "artifacts" ? (
            ArtifactsTabSlot === undefined ? null : (
              <ArtifactsTabSlot />
            )
          ) : null}
          {tab.value === "pinned" ? (
            <PinnedTab events={pinned} issueKey={issueKey} pinnedIds={pinnedIds} />
          ) : null}
          {tab.value === "comments" && visibleArtifact !== undefined ? (
            <CommentsTab
              artifactSlug={visibleArtifact.slug}
              asksPending={asksPending}
              commentsPending={commentsPending}
              composer={composer}
              hoveredItemId={selection.hoveredItemId}
              isClosed={isClosed}
              issueKey={issueKey ?? ""}
              items={comments}
              list={commentListRef}
              onAction={actions.onAction}
              onCloseComposer={actions.closeComposer}
              onReply={actions.onReply}
              selectedItemId={selection.selectedItemId}
            />
          ) : null}
        </div>
      </aside>
    </>
  );
}
