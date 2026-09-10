import { type ReactNode, useRef } from "react";

import { QueryError } from "../../components/QueryError";
import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  backdrop50,
  borderDefault,
  card,
  dragHandleBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog, useMediaQuery } from "../shell/useDialog";
import { CommentsTab } from "./CommentsTab";
import type { MarginSheetModel } from "./Margin";
import { PinnedTab } from "./PinnedTab";
import type { MarginTab } from "./useMarginItems";

interface MarginSheetProps {
  model: MarginSheetModel;
}

export function MarginSheet({ model }: MarginSheetProps): ReactNode {
  const {
    actions,
    composer,
    items: {
      actionErrorId,
      answeredAsksPending,
      asksPending,
      comments,
      commentsError,
      commentsPending,
      marginRef,
      isClosed,
      issueError,
      issueKey,
      issuePending,
      openAskCount,
      pendingActionId,
      pinned,
      pinnedIds,
      needsYou,
      visibleArtifact,
    },
    selection,
    sheet,
    tab,
  } = model;
  const isCompactViewport = useMediaQuery("(max-width: 1279px)");
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const dialog = useDialog<HTMLElement>({
    onClose: () => sheet.toggle(false),
    open: sheet.expanded && isCompactViewport,
  });

  const reviewToggleLabel = `${sheet.expanded ? "Close" : "Open"} review panel (${openAskCount} open ${
    openAskCount === 1 ? "ask" : "asks"
  })`;

  return (
    <>
      {sheet.expanded && isCompactViewport ? (
        <div
          aria-hidden="true"
          className={`fixed inset-0 z-[9] ${backdrop50}`}
          onClick={() => sheet.toggle(false)}
        />
      ) : null}
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: aria-modal only applies while the compact review sheet is an open dialog */}
      <aside
        aria-label={isCompactViewport ? "Review panel" : "Review margin"}
        aria-modal={sheet.expanded && isCompactViewport ? true : undefined}
        className={`fixed inset-x-0 bottom-0 z-10 border-t shadow-[0_-8px_24px_rgba(15,23,42,0.08)] ${card} ${
          issueKey === undefined
            ? "hidden"
            : sheet.expanded
              ? "max-h-[85dvh] overflow-y-auto p-4"
              : "h-16 overflow-hidden"
        } xl:sticky xl:top-0 xl:order-3 xl:block xl:h-auto xl:max-h-dvh xl:w-96 xl:overflow-y-auto xl:border-t-0 xl:border-l xl:p-4 xl:shadow-none`}
        data-expanded={sheet.expanded ? "true" : "false"}
        data-testid="margin-sheet"
        ref={(element) => {
          dialog.containerRef.current = element;
          marginRef.current = element;
        }}
        role={sheet.expanded && isCompactViewport ? "dialog" : undefined}
      >
        {isCompactViewport ? (
          <>
            <div
              aria-hidden="true"
              className={`mx-auto mb-1 h-1 w-10 rounded-full ${dragHandleBg}`}
            />
            <button
              aria-expanded={sheet.expanded}
              aria-label={reviewToggleLabel}
              className={`flex h-16 w-full items-center justify-between px-4 text-left text-sm font-semibold ${textPrimaryOnSurface}`}
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
              <span>Review ({openAskCount})</span>
            </button>
          </>
        ) : null}
        <div className={sheet.expanded ? "px-4 pb-4 xl:px-0 xl:pb-0" : "hidden xl:block"}>
          <div className={`flex border-b ${borderDefault}`} role="tablist">
            {(["comments", "pinned"] as MarginTab[]).map((name) => (
              <button
                aria-selected={tab.value === name}
                className={
                  tab.value === name
                    ? `border-b-2 px-3 py-2 text-sm font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                    : `px-3 py-2 text-sm ${textSecondaryOnSurface}`
                }
                key={name}
                onClick={() => tab.set(name)}
                role="tab"
                type="button"
              >
                {name === "comments" ? "Comments" : "Pinned"}
              </button>
            ))}
          </div>
          {issueKey === undefined ? (
            <p className={`pt-3 text-sm ${textMutedOnSurface}`}>
              Open an issue to review its margin.
            </p>
          ) : null}
          {issueKey !== undefined && visibleArtifact === undefined && issuePending ? (
            <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading margin…</p>
          ) : null}
          {issueKey !== undefined && visibleArtifact === undefined && issueError ? (
            <div className="pt-3">
              <QueryError
                message="Could not load this issue's margin."
                onRetry={actions.onRetryIssue}
              />
            </div>
          ) : null}
          {tab.value === "pinned" ? (
            <PinnedTab events={pinned} issueKey={issueKey} pinnedIds={pinnedIds} />
          ) : null}
          {tab.value === "comments" && visibleArtifact !== undefined ? (
            <CommentsTab
              actionErrorId={actionErrorId}
              answeredAsksPending={answeredAsksPending}
              artifactSlug={visibleArtifact.slug}
              commentsError={commentsError}
              asksPending={asksPending}
              commentsPending={commentsPending}
              composer={composer}
              hoveredItemId={selection.hoveredItemId}
              isClosed={isClosed}
              issueKey={issueKey ?? ""}
              items={comments}
              needsYou={needsYou}
              onAction={actions.onAction}
              onCloseComposer={actions.closeComposer}
              onComposerSaved={actions.onComposerSaved}
              onReply={actions.onReply}
              onRetryAction={actions.onRetryAction}
              onRetryAnsweredAsk={actions.onRetryAnsweredAsk}
              onRetryComments={actions.onRetryComments}
              pendingActionId={pendingActionId}
              selectedItemId={selection.selectedItemId}
            />
          ) : null}
        </div>
      </aside>
    </>
  );
}
