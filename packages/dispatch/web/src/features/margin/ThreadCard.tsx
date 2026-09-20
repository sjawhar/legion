import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import type { Comment, Suggestion } from "../../api/types";
import { Pill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  calloutDangerBorder,
  calloutSuccessBorder,
  card,
  dangerHoverText,
  dangerText,
  inlineWarningText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  selectedCardBg,
  selectedCardBorder,
  successHoverText,
  successText,
  suggestionAddedBg,
  suggestionAddedText,
  suggestionRemovedBg,
  suggestionRemovedText,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionComposer } from "../conversation/MentionComposer";
import { pulseBlock } from "../doc/marks";
import { actorLabel } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, buildProjectPath, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { isBareReferenceBody, Unfurl } from "../refs/Unfurl";
import type { MarginItemAction, MarginOwner, Thread } from "./useMarginItems";

/** 44 px tall through tablet widths (the compact sheet's `min-height: 44px` rule in `styles.css`
 *  holds until `xl`), 32 px in the desktop margin. */
const suggestionActionButton =
  "min-h-11 rounded-lg border px-3 py-2 text-xs font-semibold disabled:cursor-not-allowed xl:min-h-8 xl:py-1";

/** The replacement a suggestion proposes: the anchored text struck through, the new text
 *  beneath. `clamp` keeps a collapsed card's preview to two lines of each. */
function SuggestionDiff({
  clamp,
  quote,
  suggestion,
}: {
  clamp: boolean;
  quote: string;
  suggestion: Suggestion;
}): ReactNode {
  const clampClass = clamp ? "line-clamp-2" : "";
  return (
    <>
      <del
        className={`block rounded px-2 py-1 ${clampClass} ${suggestionRemovedBg} ${suggestionRemovedText}`}
      >
        {quote}
      </del>
      <ins
        className={`block rounded px-2 py-1 ${clampClass} ${suggestionAddedBg} ${suggestionAddedText}`}
      >
        {suggestion.replace_with}
      </ins>
    </>
  );
}

export interface ThreadCardProps {
  actionError: boolean;
  artifactSlug: string | undefined;
  className?: string;
  composerClassName?: string;
  /** A docked Conversation reply owns composition, so this card keeps the reply thread visible. */
  hideReplyComposer?: boolean;
  expanded: boolean;
  hovered: boolean;
  isClosed: boolean;
  onAction(id: string, action: MarginItemAction): void;
  onEdit(id: string, body: string): Promise<unknown>;
  onRetryAction(): void;
  onSelect?(): void;
  onToggle(): void;
  owner: MarginOwner;
  /** This thread's action is saving, or waiting its turn behind another thread's save. The
   *  buttons stay enabled meanwhile - a disabled button drops the focus it holds and turns a
   *  click into a silent no-op; the margin queues the click instead. */
  pendingAction: boolean;
  /** Renders the root and reply delivery attempts where the owner has event-sourced deliveries. */
  renderDeliveries?(comment: Comment): ReactNode;
  /** Conversation owns the turn-level copy control outside this card. */
  showReference?: boolean;
  /** Document margins pulse an orphaned block; timeline cards link to the document instead. */
  pulseOrphanBlock?: boolean;
  thread: Thread;
  viewerLogin: string;
  /** The comment whose inline editor is open; owned by the margin so a card that moves between
   *  the anchored and discussion lists (a remount) keeps an edit in progress. */
  editingCommentId: string | undefined;
  onEditingChange(id: string | undefined): void;
}

function CommentBody({
  artifactSlug,
  comment,
  owner,
  showOrphan,
  showReference,
}: {
  artifactSlug: string | undefined;
  comment: Comment;
  owner: MarginOwner;
  showOrphan: boolean;
  showReference: boolean;
}): ReactNode {
  const anchor = comment.anchor;
  const suggestion = comment.suggestion;
  const reference = itemRoute("comment", comment, owner.kind === "document" ? owner : undefined);
  let orphanNotice: ReactNode = null;
  if (showOrphan && anchor?.orphaned) {
    const originalPath =
      comment.issue_key === null
        ? owner.kind === "document"
          ? buildProjectPath({
              kind: "document",
              project: owner.project,
              slug: owner.slug,
              version: anchor.version,
            })
          : undefined
        : artifactSlug === undefined
          ? undefined
          : buildIssuePath({
              key: comment.issue_key,
              kind: "artifact",
              slug: artifactSlug,
              version: anchor.version,
            });
    orphanNotice =
      originalPath === undefined ? null : (
        <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
          Text changed.{" "}
          <Link className="underline" to={`${originalPath}&comment=${comment.id}`}>
            View original text
          </Link>
        </p>
      );
  }
  return (
    <>
      {anchor === null ? null : (
        <blockquote className={`mb-2 border-l-2 pl-2 ${quoteAccentBorder} ${quoteBodyText}`}>
          {anchor.quote}
        </blockquote>
      )}
      {orphanNotice}
      {suggestion === null ? <MarkdownBody markdown={comment.body} /> : null}
      {suggestion !== null && anchor !== null ? (
        <div className="space-y-1 font-mono text-xs">
          <SuggestionDiff clamp={false} quote={anchor.quote} suggestion={suggestion} />
          {comment.body === "Suggested replacement." ? null : (
            <div className={`font-sans ${textSecondaryOnSurface}`}>
              <MarkdownBody markdown={comment.body} />
            </div>
          )}
        </div>
      ) : null}
      {isBareReferenceBody(comment.body) ? <Unfurl body={comment.body} /> : null}
      <p className={`mt-2 flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurfaceMuted}`}>
        <span>
          {actorLabel(comment.author)} · <Timestamp at={comment.created_at} />
          {comment.edited_at === null ? null : " · edited"}
        </span>
        {showReference && reference !== undefined ? <CopyRefButton route={reference} /> : null}
      </p>
    </>
  );
}

export function ThreadCard({
  actionError,
  artifactSlug,
  className,
  composerClassName,
  hideReplyComposer = false,
  expanded,
  hovered,
  isClosed,
  onAction,
  onEdit,
  onRetryAction,
  onSelect,
  onToggle,
  owner,
  showReference = true,
  pendingAction,
  pulseOrphanBlock = true,
  renderDeliveries,
  thread,
  viewerLogin,
  editingCommentId: editingId,
  onEditingChange: setEditingId,
}: ThreadCardProps): ReactNode {
  const [savingEdit, setSavingEdit] = useState(false);
  const root = thread.root.comment;
  const rootSuggestion = root.suggestion;
  const terminalSuggestion = rootSuggestion !== null && rootSuggestion.accepted !== null;
  // The server refuses accept and reject on an orphaned anchor (409 ANCHOR_ORPHANED), so the
  // card offers neither; the thread is closed with Resolve like a comment.
  const actionableSuggestion =
    !isClosed &&
    rootSuggestion !== null &&
    rootSuggestion.accepted === null &&
    !thread.resolved &&
    root.anchor?.orphaned !== true;
  const resolutionLabel =
    rootSuggestion?.accepted === true
      ? "Accepted"
      : rootSuggestion?.accepted === false
        ? "Rejected"
        : "Resolved";
  const renderEditor = (comment: Comment) => (
    <MentionComposer
      edit={{ body: comment.body, id: comment.id }}
      kind="comment"
      onClose={() => setEditingId(undefined)}
      onSent={() => setEditingId(undefined)}
      owner={
        owner.kind === "issue"
          ? { issueKey: owner.key, kind: "issue" }
          : { artifactId: owner.artifactId, kind: "artifact", project: owner.project }
      }
      saveEdit={async (id, body) => {
        setSavingEdit(true);
        try {
          return await onEdit(id, body);
        } finally {
          setSavingEdit(false);
        }
      }}
    />
  );
  const editButton = (comment: Comment) =>
    !savingEdit &&
    !isClosed &&
    comment.author.kind === "user" &&
    comment.author.id === viewerLogin ? (
      <button
        className={`font-medium ${linkText} ${linkHoverText}`}
        onClick={() => setEditingId(comment.id)}
        type="button"
      >
        Edit
      </button>
    ) : null;

  return (
    <>
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: the card exposes its expand/collapse state to the margin bridge */}
      <article
        aria-busy={pendingAction || undefined}
        aria-current={expanded ? "true" : undefined}
        aria-expanded={expanded}
        className={`rounded-xl border p-3 text-sm shadow-sm ${
          expanded || hovered ? `${selectedCardBorder} ${selectedCardBg}` : card
        } ${className ?? ""}`}
        data-hovered={hovered ? "true" : undefined}
        data-anchor-block={thread.anchor?.block_id ?? undefined}
        onClickCapture={() => {
          if (
            pulseOrphanBlock &&
            thread.anchor?.orphaned &&
            typeof thread.anchor.block_id === "string"
          ) {
            pulseBlock(thread.anchor.block_id);
          }
        }}
        onKeyDown={(event) => {
          if (
            event.target !== event.currentTarget ||
            (event.key !== "Enter" && event.key !== " ")
          ) {
            return;
          }
          event.preventDefault();
          onSelect?.();
        }}
        onClick={(event) => {
          if (
            event.target instanceof Element &&
            event.target.closest("a, button, input, label, select, textarea") !== null
          ) {
            return;
          }
          if (
            pulseOrphanBlock &&
            thread.anchor?.orphaned &&
            typeof thread.anchor.block_id === "string"
          ) {
            pulseBlock(thread.anchor.block_id);
          }
          event.stopPropagation();
          onSelect?.();
        }}
        data-margin-item={thread.key}
        data-testid={`margin-comment-${thread.key}`}
      >
        {rootSuggestion === null ? null : (
          <header className="mb-2 flex flex-wrap items-center justify-between gap-2">
            <Pill>Suggestion</Pill>
            {actionableSuggestion ? (
              <span className="flex items-center gap-2">
                <button
                  aria-label="Accept suggestion"
                  className={`${suggestionActionButton} ${calloutSuccessBorder} ${successText} ${successHoverText}`}
                  onClick={() => onAction(root.id, "accept")}
                  type="button"
                >
                  Accept
                </button>
                <button
                  aria-label="Reject suggestion"
                  className={`${suggestionActionButton} ${calloutDangerBorder} ${dangerText} ${dangerHoverText}`}
                  onClick={() => onAction(root.id, "reject")}
                  type="button"
                >
                  Reject
                </button>
              </span>
            ) : null}
          </header>
        )}
        {expanded ? (
          <>
            {editingId === root.id ? (
              renderEditor(root)
            ) : (
              <>
                <CommentBody
                  artifactSlug={artifactSlug}
                  comment={root}
                  owner={owner}
                  showOrphan
                  showReference={showReference}
                />
                {renderDeliveries?.(root)}
              </>
            )}
            {isClosed ? null : (
              <div className="mt-2 flex flex-wrap items-center gap-3 text-sm">
                {actionableSuggestion ? null : thread.resolved && !terminalSuggestion ? (
                  <button
                    className={`font-medium ${linkText} ${linkHoverText}`}
                    onClick={() => onAction(root.id, "reopen")}
                    type="button"
                  >
                    Reopen
                  </button>
                ) : !thread.resolved ? (
                  <button
                    className={`font-medium ${linkText} ${linkHoverText}`}
                    onClick={() => onAction(root.id, "resolve")}
                    type="button"
                  >
                    Resolve
                  </button>
                ) : null}
                {editButton(root)}
                {pendingAction ? (
                  <span className={`text-xs ${textMutedOnSurfaceMuted}`} role="status">
                    Saving…
                  </span>
                ) : null}
              </div>
            )}
            {thread.resolved && root.resolved_by !== null && root.resolved_at !== null ? (
              <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                {resolutionLabel} by {actorLabel(root.resolved_by)} ·{" "}
                <Timestamp at={root.resolved_at} />
              </p>
            ) : null}
            {thread.replies.length === 0 ? null : (
              <ol aria-label="Replies" className={`mt-3 space-y-2 border-t pt-3 ${borderDefault}`}>
                {thread.replies.map((reply) => (
                  <li
                    className={`rounded-lg p-2 ${card}`}
                    key={reply.id}
                    style={{ marginLeft: "0px" }}
                  >
                    {editingId === reply.id ? (
                      renderEditor(reply)
                    ) : (
                      <>
                        <CommentBody
                          artifactSlug={artifactSlug}
                          comment={reply}
                          owner={owner}
                          showOrphan={false}
                          showReference={showReference}
                        />
                        {renderDeliveries?.(reply)}
                      </>
                    )}
                    {editingId === reply.id ? null : (
                      <div className="mt-2 flex gap-3 text-sm">{editButton(reply)}</div>
                    )}
                  </li>
                ))}
              </ol>
            )}
            {isClosed || terminalSuggestion || hideReplyComposer ? null : (
              <div className={composerClassName}>
                <MentionComposer
                  inline
                  kind="comment"
                  onCancelReply={onToggle}
                  onClose={onToggle}
                  onSent={() => {}}
                  owner={
                    owner.kind === "issue"
                      ? { issueKey: owner.key, kind: "issue" }
                      : { artifactId: owner.artifactId, kind: "artifact", project: owner.project }
                  }
                  replyTo={{ author: "", excerpt: "", id: root.id, parentKind: "comment" }}
                />
              </div>
            )}
          </>
        ) : (
          <button
            aria-expanded={false}
            className={`block w-full text-left ${textPrimaryOnSurface}`}
            onClick={() => {
              if (
                pulseOrphanBlock &&
                thread.anchor?.orphaned &&
                typeof thread.anchor.block_id === "string"
              ) {
                pulseBlock(thread.anchor.block_id);
              }
              onToggle();
            }}
            type="button"
          >
            {/* The compact sheet's stylesheet lays every button out inline-flex, so one block
                wrapper keeps the preview's rows stacked. */}
            <span className="block w-full">
              {rootSuggestion !== null && root.anchor !== null ? (
                <>
                  {root.anchor.orphaned ? (
                    <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>Text changed.</p>
                  ) : null}
                  <span className="block space-y-1 font-mono text-xs">
                    <SuggestionDiff clamp quote={root.anchor.quote} suggestion={rootSuggestion} />
                  </span>
                  {root.body === "Suggested replacement." ? null : (
                    <p className={`mt-2 line-clamp-1 text-xs ${textSecondaryOnSurface}`}>
                      <MarkdownBody markdown={root.body} variant="inline" />
                    </p>
                  )}
                </>
              ) : (
                <>
                  {root.anchor === null ? null : (
                    <blockquote
                      className={`mb-2 border-l-2 pl-2 ${quoteAccentBorder} ${quoteBodyText}`}
                    >
                      {root.anchor.quote}
                    </blockquote>
                  )}
                  <p className="line-clamp-2">
                    <MarkdownBody markdown={root.body} variant="inline" />
                  </p>
                  {isBareReferenceBody(root.body) ? <Unfurl body={root.body} /> : null}
                </>
              )}
              {thread.replies.length === 0 ? null : (
                <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                  {thread.replies.length} {thread.replies.length === 1 ? "reply" : "replies"} · last
                  reply{" "}
                  {thread.lastReplyAt === undefined ? null : <Timestamp at={thread.lastReplyAt} />}
                </p>
              )}
            </span>
          </button>
        )}
        {expanded ? null : renderDeliveries?.(root)}
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
    </>
  );
}
