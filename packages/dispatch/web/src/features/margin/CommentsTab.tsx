import type { ReactNode } from "react";

import type { Ask } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { QueryError } from "../../components/QueryError";
import { borderDefault, highlightRing, textPrimaryOnSurface } from "../../theme/classes";
import {
  type ComposerAnchor,
  type ComposerKind,
  MentionComposer,
} from "../conversation/MentionComposer";
import { AskCard } from "../inbox/AskCard";
import { isBareReferenceBody, Unfurl } from "../refs/Unfurl";
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
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  needsYou: Ask[];
  onAction: (id: string, action: MarginItemAction) => void;
  onCloseComposer: () => void;
  onComposerSaved: () => void;
  onEdit: (id: string, body: string) => Promise<unknown>;
  onRetryAction: () => void;
  onRetryAnsweredAsk: (() => void) | undefined;
  onRetryComments: () => void;
  onSelectCard: (id: string, blockID: string | undefined) => void;
  onToggleThread: (key: string) => void;
  onToggleResolved: () => void;
  pendingActionIds: ReadonlySet<string>;
  resolvedThreads: Thread[];
  retractedAskCount: number;
  selectedItemId: string | undefined;
  showResolved: boolean;
  threads: Thread[];
  viewerLogin: string;
}

export function MarginAskCard({
  artifactSlug,
  ask,
  owner,
  selected,
}: {
  artifactSlug: string;
  ask: Ask;
  owner: MarginOwner;
  selected: boolean;
}): ReactNode {
  return (
    <div
      aria-current={selected ? "true" : undefined}
      className={selected ? `rounded-xl ${highlightRing}` : undefined}
      data-margin-item={ask.id}
    >
      <AskCard
        ask={ask}
        artifactSlug={artifactSlug}
        owner={owner.kind === "document" ? owner : undefined}
        variant="compact"
      />
      {isBareReferenceBody(ask.question) ? <Unfurl body={ask.question} /> : null}
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
  blockPlacements,
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
  pendingActionIds,
  resolvedThreads,
  retractedAskCount,
  selectedItemId,
  showResolved,
  threads,
  viewerLogin,
}: CommentsTabProps): ReactNode {
  return (
    <div className="space-y-3 pt-3">
      {composer === undefined || isClosed ? null : (
        <MentionComposer
          anchor={composer.anchor}
          autoFocus
          kind={composer.kind}
          onClose={onCloseComposer}
          onSent={onComposerSaved}
          owner={
            owner.kind === "issue"
              ? { issueKey: owner.key, kind: "issue" }
              : { artifactId: owner.artifactId, kind: "artifact", project: owner.project }
          }
          replyTo={
            composer.replyTo === undefined
              ? null
              : { author: "", excerpt: "", id: composer.replyTo, parentKind: "comment" }
          }
          showKindSwitch={composer.anchor !== undefined}
        />
      )}
      <section aria-label="Margin review items" className="space-y-3">
        {needsYou.length === 0 ? null : (
          <section aria-label="Needs you" className={`space-y-3 border-b pb-3 ${borderDefault}`}>
            <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Needs you</h2>
            {needsYou.map((ask) => (
              <MarginAskCard
                artifactSlug={artifactSlug}
                ask={ask}
                key={ask.id}
                owner={owner}
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
              <MarginAskCard
                artifactSlug={artifactSlug}
                ask={ask}
                key={ask.id}
                owner={owner}
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
              blockPlacements={blockPlacements}
              markPlacements={markPlacements}
              onAction={onAction}
              onEdit={onEdit}
              onRetryAction={onRetryAction}
              onSelect={onSelectCard}
              onToggle={onToggleThread}
              onToggleResolved={onToggleResolved}
              pendingActionIds={pendingActionIds}
              resolvedThreads={resolvedThreads}
              retractedAskCount={retractedAskCount}
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
              <EmptyState
                label="Margin review empty state"
                message="No comments, asks, or suggestions on this document."
              />
            ) : null}
          </>
        )}
      </section>
    </div>
  );
}
