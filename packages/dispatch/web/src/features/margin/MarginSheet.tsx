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
import { ThreadCard } from "./ThreadCard";
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
      commentsError,
      commentsPending,
      historicalAsks,
      marginRef,
      isClosed,
      issueError,
      owner,
      issuePending,
      openAskCount,
      pendingActionId,
      pinned,
      pinnedIds,
      needsYou,
      onSelectCard,
      resolvedThreads,
      threads,
      viewerLogin,
      visibleArtifact,
    },
    filter,
    placement,
    selection,
    sheet,
    tab,
  } = model;
  const isCompactViewport = useMediaQuery("(max-width: 1279px)");
  const isPhoneViewport = useMediaQuery("(max-width: 767px)");
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const dialog = useDialog<HTMLElement>({
    onClose: () => sheet.toggle(false),
    open: sheet.expanded && isCompactViewport,
  });

  const reviewToggleLabel = `${sheet.expanded ? "Close" : "Open"} review panel (${openAskCount} open ${
    openAskCount === 1 ? "ask" : "asks"
  })`;
  const phoneThread =
    sheet.threadKey === undefined
      ? undefined
      : [...threads, ...resolvedThreads].find((thread) => thread.key === sheet.threadKey);
  if (phoneThread !== undefined && visibleArtifact === undefined) {
    throw new Error("A phone margin thread requires its visible artifact.");
  }
  const threadDialog = useDialog<HTMLElement>({
    onClose: sheet.closeThread,
    open: isPhoneViewport && phoneThread !== undefined,
  });

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
          owner === undefined
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
        {!isPhoneViewport || phoneThread === undefined ? (
          <div className={sheet.expanded ? "px-4 pb-4 xl:px-0 xl:pb-0" : "hidden xl:block"}>
            <div className={`flex border-b ${borderDefault}`} role="tablist">
              {(owner?.kind === "document"
                ? (["comments"] as MarginTab[])
                : (["comments", "pinned"] as MarginTab[])
              ).map((name) => (
                <button
                  aria-selected={tab.value === name}
                  className={
                    tab.value === name
                      ? `min-h-11 border-b-2 px-3 py-2 text-sm font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                      : `min-h-11 px-3 py-2 text-sm ${textSecondaryOnSurface}`
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
            {owner === undefined ? (
              <p className={`pt-3 text-sm ${textMutedOnSurface}`}>
                Open an issue or document to review its margin.
              </p>
            ) : null}
            {owner?.kind === "issue" && visibleArtifact === undefined && issuePending ? (
              <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading margin…</p>
            ) : null}
            {owner?.kind === "issue" && visibleArtifact === undefined && issueError ? (
              <div className="pt-3">
                <QueryError
                  message="Could not load this issue's margin."
                  onRetry={actions.onRetryIssue}
                />
              </div>
            ) : null}
            {tab.value === "pinned" && owner?.kind === "issue" ? (
              <PinnedTab events={pinned} issueKey={owner.key} pinnedIds={pinnedIds} />
            ) : null}
            {filter.blockId === undefined ? null : (
              <div
                className={`mt-3 flex items-center justify-between gap-2 text-sm ${textMutedOnSurface}`}
              >
                <span>References for this block</span>
                <button
                  className={`min-h-11 font-medium ${textPrimaryOnSurface}`}
                  onClick={filter.clear}
                  type="button"
                >
                  Clear filter
                </button>
              </div>
            )}
            {tab.value === "comments" && visibleArtifact !== undefined && owner !== undefined ? (
              <CommentsTab
                actionErrorId={actionErrorId}
                answeredAsksPending={answeredAsksPending}
                artifactSlug={visibleArtifact.slug}
                asksPending={asksPending}
                commentsError={commentsError}
                commentsPending={commentsPending}
                composer={composer}
                expandedThreadKey={selection.expandedThreadKey}
                editingCommentId={selection.editingCommentId}
                onEditingChange={actions.onEditingChange}
                historicalAsks={historicalAsks}
                hoveredItemId={selection.hoveredItemId}
                hoveredMarkId={selection.hoveredMarkId}
                isClosed={isClosed}
                owner={owner}
                markPlacements={placement.markPlacements}
                blockPlacements={placement.blockPlacements}
                needsYou={needsYou}
                onAction={actions.onAction}
                onCloseComposer={actions.closeComposer}
                onComposerSaved={actions.onComposerSaved}
                onEdit={actions.onEdit}
                onRetryAction={actions.onRetryAction}
                onRetryAnsweredAsk={actions.onRetryAnsweredAsk}
                onRetryComments={actions.onRetryComments}
                onSelectCard={onSelectCard}
                onToggleResolved={actions.onToggleResolved}
                onToggleThread={actions.onToggleThread}
                pendingActionId={pendingActionId}
                resolvedThreads={resolvedThreads}
                selectedItemId={selection.selectedItemId}
                showResolved={selection.showResolved}
                threads={threads}
                viewerLogin={viewerLogin}
              />
            ) : null}
          </div>
        ) : null}
        {isPhoneViewport &&
        phoneThread !== undefined &&
        owner !== undefined &&
        visibleArtifact !== undefined ? (
          <section
            aria-label="Thread"
            aria-modal="true"
            className={`fixed inset-0 z-20 flex flex-col ${card}`}
            role="dialog"
            ref={threadDialog.containerRef}
          >
            <header className={`flex items-center border-b px-4 py-3 ${borderDefault}`}>
              <button
                className={`min-h-11 text-sm font-medium ${textPrimaryOnSurface}`}
                onClick={sheet.closeThread}
                type="button"
              >
                Back
              </button>
            </header>
            <div className="min-h-0 flex-1 overflow-y-auto px-4">
              <ThreadCard
                actionError={actionErrorId === phoneThread.key}
                artifactSlug={visibleArtifact.slug}
                className="min-h-full pb-32"
                composerClassName={`fixed inset-x-0 bottom-0 z-10 border-t px-4 pt-4 pb-2 ${card} ${borderDefault}`}
                expanded
                hovered={selection.hoveredMarkId === phoneThread.anchor?.mark_id}
                editingCommentId={selection.editingCommentId}
                onEditingChange={actions.onEditingChange}
                isClosed={isClosed}
                onAction={actions.onAction}
                onEdit={actions.onEdit}
                onRetryAction={actions.onRetryAction}
                onSelect={() =>
                  onSelectCard(phoneThread.key, phoneThread.anchor?.block_id ?? undefined)
                }
                onToggle={sheet.closeThread}
                owner={owner}
                pendingAction={pendingActionId === phoneThread.key}
                thread={phoneThread}
                viewerLogin={viewerLogin}
              />
            </div>
          </section>
        ) : null}
      </aside>
    </>
  );
}
