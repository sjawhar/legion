import type { ReactNode } from "react";

import type { Ask } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  highlightRing,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { Timestamp } from "../refs/Timestamp";
import { isBareReferenceBody, Unfurl } from "../refs/Unfurl";
import { Composer, type ComposerAnchor, type ComposerKind } from "./Composer";
import { ThreadList } from "./ThreadList";
import type { MarginItemAction, MarginOwner, MarkPlacement, Thread } from "./useMarginItems";

export interface MarginComposer {
  anchor: ComposerAnchor | undefined;
  kind: ComposerKind;
  replyTo?: string;
}

interface CommentsTabProps {
  actionErrorId: string | undefined;
  answeredAsksPending: boolean;
  artifactSlug: string;
  asksPending: boolean;
  commentsError: boolean;
  commentsPending: boolean;
  composer: MarginComposer | undefined;
  expandedThreadKey: string | undefined;
  editingCommentId: string | undefined;
  onEditingChange(id: string | undefined): void;
  historicalAsks: Ask[];
  hoveredItemId: string | undefined;
  hoveredMarkId: string | undefined;
  isClosed: boolean;
  owner: MarginOwner;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  needsYou: Ask[];
  onAction: (id: string, action: MarginItemAction) => void;
  onCloseComposer: () => void;
  onComposerSaved: () => void;
  onEdit: (id: string, body: string) => Promise<unknown>;
  onRetryAction: () => void;
  onRetryAnsweredAsk: (() => void) | undefined;
  onRetryComments: () => void;
  onSelectCard: (id: string) => void;
  onToggleThread: (key: string) => void;
  onToggleResolved: () => void;
  pendingActionId: string | undefined;
  resolvedThreads: Thread[];
  selectedItemId: string | undefined;
  showResolved: boolean;
  threads: Thread[];
  viewerLogin: string;
}

function AskCardItem({
  artifactSlug,
  ask,
  selected,
}: {
  artifactSlug: string;
  ask: Ask;
  selected: boolean;
}): ReactNode {
  return (
    <div
      aria-current={selected ? "true" : undefined}
      className={selected ? `rounded-xl ${highlightRing}` : undefined}
      data-margin-item={ask.id}
    >
      <AskCard ask={ask} artifactSlug={artifactSlug} />
      {isBareReferenceBody(ask.question) ? <Unfurl body={ask.question} /> : null}
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        <Timestamp at={ask.created_at} />
      </p>
    </div>
  );
}

export function CommentsTab({
  actionErrorId,
  answeredAsksPending,
  artifactSlug,
  asksPending,
  commentsError,
  commentsPending,
  composer,
  expandedThreadKey,
  editingCommentId,
  onEditingChange,
  historicalAsks,
  hoveredItemId,
  hoveredMarkId,
  isClosed,
  owner,
  markPlacements,
  needsYou,
  onAction,
  onCloseComposer,
  onComposerSaved,
  onEdit,
  onRetryAction,
  onRetryAnsweredAsk,
  onRetryComments,
  onSelectCard,
  onToggleResolved,
  onToggleThread,
  pendingActionId,
  resolvedThreads,
  selectedItemId,
  showResolved,
  threads,
  viewerLogin,
}: CommentsTabProps): ReactNode {
  return (
    <div className="space-y-3 pt-3">
      {composer === undefined ||
      isClosed ||
      (owner.kind === "document" && composer.kind === "message") ? null : (
        <Composer
          anchor={composer.anchor}
          autoFocus
          kind={composer.kind}
          owner={owner}
          onClose={onCloseComposer}
          onSaved={onComposerSaved}
          replyTo={composer.replyTo}
        />
      )}
      <section aria-label="Margin review items" className="space-y-3">
        {needsYou.length === 0 ? null : (
          <section aria-label="Needs you" className={`space-y-3 border-b pb-3 ${borderDefault}`}>
            <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Needs you</h2>
            {needsYou.map((ask) => (
              <AskCardItem
                artifactSlug={artifactSlug}
                ask={ask}
                key={ask.id}
                selected={selectedItemId === ask.id || hoveredItemId === ask.id}
              />
            ))}
          </section>
        )}
        {onRetryAnsweredAsk === undefined ? null : (
          <QueryError
            message="Could not load this document's asks."
            onRetry={onRetryAnsweredAsk}
            retrying={answeredAsksPending}
          />
        )}
        {commentsError ? (
          <QueryError
            message="Could not load this document's comments."
            onRetry={onRetryComments}
          />
        ) : (
          <>
            {historicalAsks.map((ask) => (
              <AskCardItem
                artifactSlug={artifactSlug}
                ask={ask}
                key={ask.id}
                selected={selectedItemId === ask.id || hoveredItemId === ask.id}
              />
            ))}
            <ThreadList
              actionErrorId={actionErrorId}
              artifactSlug={artifactSlug}
              expandedThreadKey={expandedThreadKey}
              editingCommentId={editingCommentId}
              onEditingChange={onEditingChange}
              hoveredItemId={hoveredItemId}
              hoveredMarkId={hoveredMarkId}
              isClosed={isClosed}
              markPlacements={markPlacements}
              onAction={onAction}
              onEdit={onEdit}
              onRetryAction={onRetryAction}
              onSelect={onSelectCard}
              onToggle={onToggleThread}
              onToggleResolved={onToggleResolved}
              pendingActionId={pendingActionId}
              resolvedThreads={resolvedThreads}
              showResolved={showResolved}
              owner={owner}
              threads={threads}
              viewerLogin={viewerLogin}
            />
            {threads.length === 0 &&
            resolvedThreads.length === 0 &&
            historicalAsks.length === 0 &&
            needsYou.length === 0 &&
            !asksPending &&
            !commentsPending &&
            !answeredAsksPending ? (
              <p className={`text-sm ${textMutedOnSurface}`}>
                No comments, asks, or suggestions on this document.
              </p>
            ) : null}
          </>
        )}
      </section>
    </div>
  );
}
