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
import { MARGIN_COMPOSER_SEND_KEY } from "./margin-context";
import { ThreadList } from "./ThreadList";
import type { CommentActionFailure } from "./useCommentActionQueue";
import type { MarginItemAction, MarginOwner, MarkPlacement, Thread } from "./useMarginItems";

export interface MarginComposer {
  anchor: ComposerAnchor | undefined;
  kind: ComposerKind;
  replyTo?: string;
}

interface CommentsTabProps {
  actionFailure: CommentActionFailure | undefined;
  answeredAsksPending: boolean;
  artifactSlug: string;
  asksPending: boolean;
  commentsError: boolean;
  commentsPending: boolean;
  expandedThreadKey: string | undefined;
  editingCommentId: string | undefined;
  savingCommentEditId: string | undefined;
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

/** The composer a selection-bar action (or a margin Reply) opened. `MarginSheet` keeps it mounted
 *  for as long as its compose is open, whatever the margin shows over it - the Pinned tab, a phone
 *  margin thread, the rail the margin collapses to - so its draft, a send it has out and that
 *  send's refusal stay with it; on a closed issue it shows only that send or its refusal. */
export function MarginComposerSlot({
  composer,
  hidden,
  isClosed,
  onClose,
  onKindChange,
  onSent,
  owner,
}: {
  composer: MarginComposer;
  hidden: boolean;
  isClosed: boolean;
  onClose: () => void;
  onKindChange: (kind: ComposerKind) => string | undefined;
  onSent: () => void;
  owner: MarginOwner;
}): ReactNode {
  return (
    // The margin scrolls this into its scrollport when it opens: a composer the reader started
    // from the document renders at the top of the margin's scroll content, which can be thousands
    // of pixels above wherever the margin is parked. `empty:hidden`: a closed issue's composer
    // with nothing to show renders nothing.
    <div className="pt-3 empty:hidden" data-margin-composer="" hidden={hidden}>
      <MentionComposer
        anchor={composer.anchor}
        autoFocus
        closed={isClosed}
        kind={composer.kind}
        mutationKey={MARGIN_COMPOSER_SEND_KEY}
        onClose={onClose}
        onSent={onSent}
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
        onKindChange={composer.anchor === undefined ? undefined : onKindChange}
      />
    </div>
  );
}

export function MarginAskCard({
  artifactSlug,
  ask,
  owner,
  section,
  selected,
}: {
  artifactSlug: string;
  ask: Ask;
  owner: MarginOwner;
  /** Which group the card is listed under: an open ask waiting on the reader, or a decided one. */
  section: "needs-you" | "decided";
  selected: boolean;
}): ReactNode {
  return (
    <div
      aria-current={selected ? "true" : undefined}
      className={selected ? `rounded-xl ${highlightRing}` : undefined}
      data-margin-item={ask.id}
      data-margin-section={section}
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
  actionFailure,
  answeredAsksPending,
  artifactSlug,
  asksPending,
  commentsError,
  commentsPending,
  expandedThreadKey,
  editingCommentId,
  savingCommentEditId,
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
  // Every ask card, open and decided, is one keyed list with the `Needs you` heading as an item
  // in it (the Inbox's pattern): an ask the reader answers moves from the open group to the
  // decided one within the same parent, so React moves its card rather than remounting it, and
  // the thread's reply the reader has started typing stays where they typed it.
  const decided = commentsError
    ? []
    : historicalAsks.filter((ask) => !needsYou.some((open) => open.id === ask.id));
  const askRows: ReactNode[] = [];
  if (needsYou.length > 0) {
    askRows.push(
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`} key="needs-you">
        Needs you
      </h2>
    );
  }
  for (const [section, asks] of [
    ["needs-you", needsYou],
    ["decided", decided],
  ] as const) {
    for (const ask of asks) {
      askRows.push(
        <MarginAskCard
          artifactSlug={artifactSlug}
          ask={ask}
          key={ask.id}
          owner={owner}
          section={section}
          selected={selectedItemId === ask.id || hoveredItemId === ask.id}
        />
      );
    }
    if (section === "needs-you" && asks.length > 0) {
      askRows.push(<hr className={borderDefault} key="needs-you-end" />);
    }
  }
  return (
    <div className="space-y-3 pt-3">
      <section aria-label="Margin review items" className="space-y-3">
        {askRows}
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
            <ThreadList
              actionFailure={actionFailure}
              artifactSlug={artifactSlug}
              expandedThreadKey={expandedThreadKey}
              editingCommentId={editingCommentId}
              savingCommentEditId={savingCommentEditId}
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
              selectedItemId={selectedItemId}
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
