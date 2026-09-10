import type { ReactNode, RefObject } from "react";
import { Link } from "react-router-dom";

import type { Ask, Comment } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  card,
  dangerHoverText,
  dangerText,
  highlightRing,
  inlineWarningText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  secondaryButtonDisabledText,
  selectedCardBg,
  selectedCardBorder,
  successHoverText,
  successText,
  suggestionAddedBg,
  suggestionAddedText,
  suggestionRemovedBg,
  suggestionRemovedText,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { actorLabel } from "../refs/actor";
import { buildIssuePath } from "../refs/routes";
import { Unfurl } from "../refs/Unfurl";
import { Composer, type ComposerAnchor, type ComposerKind } from "./Composer";
import { type MarginItem, type MarginItemAction, marginItemId } from "./useMarginItems";

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
  hoveredItemId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  items: MarginItem[];
  needsYou: Ask[];
  list: RefObject<HTMLDivElement | null>;
  onAction: (id: string, action: MarginItemAction) => void;
  onCloseComposer: () => void;
  onReply: (comment: Comment) => void;
  onRetryAction: () => void;
  onRetryAnsweredAsk: (() => void) | undefined;
  onRetryComments: () => void;
  pendingActionId: string | undefined;
  selectedItemId: string | undefined;
}

function CommentCard({
  actionError,
  artifactSlug,
  comment,
  depth,
  onAction,
  onReply,
  onRetryAction,
  pendingAction,
  selected,
}: {
  actionError: boolean;
  artifactSlug: string;
  comment: Comment;
  depth: number;
  onAction: (id: string, action: MarginItemAction) => void;
  onReply: (comment: Comment) => void;
  onRetryAction: () => void;
  pendingAction: boolean;
  selected: boolean;
}): ReactNode {
  const suggestion = comment.suggestion;
  const anchor = comment.anchor;
  return (
    <article
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        selected ? `${selectedCardBorder} ${selectedCardBg}` : card
      }`}
      data-margin-item={comment.id}
      data-testid={`margin-comment-${comment.id}`}
      style={{ marginLeft: `${depth * 12}px` }}
    >
      {anchor === null ? null : (
        <blockquote className={`mb-2 border-l-2 pl-2 ${quoteAccentBorder} ${quoteBodyText}`}>
          {anchor.quote}
        </blockquote>
      )}
      {anchor?.orphaned ? (
        <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
          Text changed.{" "}
          <Link
            className="underline"
            to={`${buildIssuePath({
              key: comment.issue_key,
              kind: "artifact",
              slug: artifactSlug,
              version: anchor.version,
            })}&comment=${comment.id}`}
          >
            View original text
          </Link>
        </p>
      ) : null}
      {suggestion === null ? <p className="whitespace-pre-wrap">{comment.body}</p> : null}
      {suggestion !== null && anchor !== null ? (
        <div className="space-y-1 font-mono text-xs">
          <del
            className={`block rounded px-2 py-1 ${suggestionRemovedBg} ${suggestionRemovedText}`}
          >
            {anchor.quote}
          </del>
          <ins className={`block rounded px-2 py-1 ${suggestionAddedBg} ${suggestionAddedText}`}>
            {suggestion.replace_with}
          </ins>
          {comment.body === "Suggested replacement." ? null : (
            <p className={`whitespace-pre-wrap font-sans ${textSecondaryOnSurface}`}>
              {comment.body}
            </p>
          )}
        </div>
      ) : null}
      <Unfurl body={comment.body} />
      <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
        {actorLabel(comment.author)} · {new Date(comment.created_at).toLocaleString()}
      </p>
      <div className="mt-2 flex flex-wrap items-center gap-3 text-sm">
        {suggestion !== null && suggestion.accepted === null && !comment.resolved ? (
          <>
            <button
              className={`font-medium disabled:cursor-not-allowed ${successText} ${successHoverText} ${secondaryButtonDisabledText}`}
              disabled={pendingAction}
              onClick={() => onAction(comment.id, "accept")}
              type="button"
            >
              Accept
            </button>
            <button
              className={`font-medium disabled:cursor-not-allowed ${dangerText} ${dangerHoverText} ${secondaryButtonDisabledText}`}
              disabled={pendingAction}
              onClick={() => onAction(comment.id, "reject")}
              type="button"
            >
              Reject
            </button>
          </>
        ) : suggestion === null && !comment.resolved ? (
          <button
            className={`font-medium disabled:cursor-not-allowed ${linkText} ${linkHoverText} ${secondaryButtonDisabledText}`}
            disabled={pendingAction}
            onClick={() => onAction(comment.id, "resolve")}
            type="button"
          >
            Resolve
          </button>
        ) : (
          <span className={`font-medium ${textMutedOnSurfaceMuted}`}>
            {suggestion?.accepted === true
              ? "Accepted"
              : suggestion?.accepted === false
                ? "Rejected"
                : "Resolved"}
          </span>
        )}
        {pendingAction ? (
          <span className={`text-xs ${textMutedOnSurfaceMuted}`} role="status">
            Saving…
          </span>
        ) : null}
        {comment.resolved ? null : (
          <button
            className={`font-medium ${linkText} ${linkHoverText}`}
            onClick={() => onReply(comment)}
            type="button"
          >
            Reply
          </button>
        )}
      </div>
      {actionError ? (
        <div className="mt-2">
          <QueryError
            message="Could not save this action."
            onRetry={onRetryAction}
            retrying={pendingAction}
          />
        </div>
      ) : null}
    </article>
  );
}

function AskCardItem({ ask, selected }: { ask: Ask; selected: boolean }): ReactNode {
  return (
    <div className={selected ? `rounded-xl ${highlightRing}` : undefined} data-margin-item={ask.id}>
      <AskCard ask={ask} />
      <Unfurl body={ask.question} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        {new Date(ask.created_at).toLocaleString()}
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
  hoveredItemId,
  isClosed,
  issueKey,
  items,
  list,
  needsYou,
  onAction,
  onCloseComposer,
  onReply,
  onRetryAction,
  onRetryAnsweredAsk,
  onRetryComments,
  pendingActionId,
  selectedItemId,
}: CommentsTabProps): ReactNode {
  return (
    <div className="space-y-3 pt-3">
      {composer === undefined || isClosed ? null : (
        <Composer
          anchor={composer.anchor}
          autoFocus
          kind={composer.kind}
          issueKey={issueKey}
          onClose={onCloseComposer}
          replyTo={composer.replyTo}
        />
      )}
      <section
        aria-label="Margin review items"
        className="space-y-3 md:max-h-[45dvh] md:overflow-y-auto"
        ref={list}
      >
        {needsYou.length === 0 ? null : (
          <section aria-label="Needs you" className={`space-y-3 border-b pb-3 ${borderDefault}`}>
            <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Needs you</h2>
            {needsYou.map((ask) => (
              <AskCardItem
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
            {items.map((item) => {
              const id = marginItemId(item);
              const active = selectedItemId === id || hoveredItemId === id;
              return (
                <div key={id}>
                  {item.kind === "ask" ? (
                    <AskCardItem ask={item.ask} selected={active} />
                  ) : (
                    <CommentCard
                      actionError={actionErrorId === item.comment.id}
                      artifactSlug={artifactSlug}
                      comment={item.comment}
                      depth={item.depth}
                      onAction={onAction}
                      onReply={onReply}
                      onRetryAction={onRetryAction}
                      pendingAction={pendingActionId === item.comment.id}
                      selected={active}
                    />
                  )}
                </div>
              );
            })}
            {items.length === 0 &&
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
