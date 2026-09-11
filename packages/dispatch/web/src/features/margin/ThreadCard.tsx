import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import type { Comment } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  card,
  dangerHoverText,
  dangerText,
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
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { actorLabel } from "../refs/actor";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { Unfurl } from "../refs/Unfurl";
import { Composer } from "./Composer";
import type { MarginItemAction, MarginOwner, Thread } from "./useMarginItems";

export interface ThreadCardProps {
  actionError: boolean;
  artifactSlug: string;
  className?: string;
  composerClassName?: string;
  expanded: boolean;
  hovered: boolean;
  isClosed: boolean;
  onAction(id: string, action: MarginItemAction): void;
  onEdit(id: string, body: string): Promise<unknown>;
  onRetryAction(): void;
  onSelect?(): void;
  onToggle(): void;
  owner: MarginOwner;
  pendingAction: boolean;
  thread: Thread;
  viewerLogin: string;
}

function CommentBody({
  artifactSlug,
  comment,
  owner,
  showOrphan,
}: {
  artifactSlug: string;
  comment: Comment;
  owner: MarginOwner;
  showOrphan: boolean;
}): ReactNode {
  const anchor = comment.anchor;
  const suggestion = comment.suggestion;
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
        {actorLabel(comment.author)} · <Timestamp at={comment.created_at} />
        {comment.edited_at === null ? null : " · edited"}
      </p>
    </>
  );
}

export function ThreadCard({
  actionError,
  artifactSlug,
  className,
  composerClassName,
  expanded,
  hovered,
  isClosed,
  onAction,
  onEdit,
  onRetryAction,
  onSelect,
  onToggle,
  owner,
  pendingAction,
  thread,
  viewerLogin,
}: ThreadCardProps): ReactNode {
  const [editingId, setEditingId] = useState<string>();
  const root = thread.root.comment;
  const rootSuggestion = root.suggestion;
  const terminalSuggestion = rootSuggestion !== null && rootSuggestion.accepted !== null;
  const resolutionLabel =
    rootSuggestion?.accepted === true
      ? "Accepted"
      : rootSuggestion?.accepted === false
        ? "Rejected"
        : "Resolved";
  const renderEditor = (comment: Comment) => (
    <Composer
      edit={{ body: comment.body, id: comment.id }}
      kind="comment"
      onClose={() => setEditingId(undefined)}
      owner={owner}
      saveEdit={onEdit}
      onSaved={() => setEditingId(undefined)}
    />
  );
  const editButton = (comment: Comment) =>
    comment.author.kind === "user" && comment.author.id === viewerLogin ? (
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
        aria-current={expanded ? "true" : undefined}
        aria-expanded={expanded}
        className={`rounded-xl border p-3 text-sm shadow-sm ${
          expanded || hovered ? `${selectedCardBorder} ${selectedCardBg}` : card
        } ${className ?? ""}`}
        data-hovered={hovered ? "true" : undefined}
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
          event.stopPropagation();
          onSelect?.();
        }}
        data-margin-item={thread.key}
        data-testid={`margin-comment-${thread.key}`}
      >
        {expanded ? (
          <>
            {editingId === root.id ? (
              renderEditor(root)
            ) : (
              <CommentBody artifactSlug={artifactSlug} comment={root} owner={owner} showOrphan />
            )}
            <div className="mt-2 flex flex-wrap items-center gap-3 text-sm">
              {rootSuggestion !== null && rootSuggestion.accepted === null && !thread.resolved ? (
                <>
                  <button
                    className={`font-medium disabled:cursor-not-allowed ${successText} ${successHoverText} ${secondaryButtonDisabledText}`}
                    disabled={pendingAction}
                    onClick={() => onAction(root.id, "accept")}
                    type="button"
                  >
                    Accept
                  </button>
                  <button
                    className={`font-medium disabled:cursor-not-allowed ${dangerText} ${dangerHoverText} ${secondaryButtonDisabledText}`}
                    disabled={pendingAction}
                    onClick={() => onAction(root.id, "reject")}
                    type="button"
                  >
                    Reject
                  </button>
                </>
              ) : thread.resolved && !terminalSuggestion ? (
                <button
                  className={`font-medium disabled:cursor-not-allowed ${linkText} ${linkHoverText} ${secondaryButtonDisabledText}`}
                  disabled={pendingAction}
                  onClick={() => onAction(root.id, "reopen")}
                  type="button"
                >
                  Reopen
                </button>
              ) : !thread.resolved ? (
                <button
                  className={`font-medium disabled:cursor-not-allowed ${linkText} ${linkHoverText} ${secondaryButtonDisabledText}`}
                  disabled={pendingAction}
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
            {thread.resolved && root.resolved_by !== null && root.resolved_at !== null ? (
              <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                {resolutionLabel} by {actorLabel(root.resolved_by)} ·{" "}
                <Timestamp at={root.resolved_at} />
              </p>
            ) : null}
            {actionError ? (
              <div className="mt-2">
                <QueryError
                  message="Could not save this action."
                  onRetry={onRetryAction}
                  retrying={pendingAction}
                />
              </div>
            ) : null}
            {thread.replies.length === 0 ? null : (
              <ol className={`mt-3 space-y-2 border-t pt-3 ${borderDefault}`}>
                {thread.replies.map((reply) => (
                  <li
                    className={`rounded-lg p-2 ${card}`}
                    key={reply.id}
                    style={{ marginLeft: "0px" }}
                  >
                    {editingId === reply.id ? (
                      renderEditor(reply)
                    ) : (
                      <CommentBody
                        artifactSlug={artifactSlug}
                        comment={reply}
                        owner={owner}
                        showOrphan={false}
                      />
                    )}
                    {editingId === reply.id ? null : (
                      <div className="mt-2 flex gap-3 text-sm">{editButton(reply)}</div>
                    )}
                  </li>
                ))}
              </ol>
            )}
            {isClosed || terminalSuggestion ? null : (
              <div className={composerClassName}>
                <Composer
                  inline
                  kind="comment"
                  onClose={onToggle}
                  owner={owner}
                  replyTo={root.id}
                />
              </div>
            )}
          </>
        ) : (
          <button
            aria-expanded={false}
            className={`block w-full text-left ${textPrimaryOnSurface}`}
            onClick={onToggle}
            type="button"
          >
            <p className="line-clamp-2 whitespace-pre-wrap">{root.body}</p>
            {thread.replies.length === 0 ? null : (
              <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>
                {thread.replies.length} {thread.replies.length === 1 ? "reply" : "replies"} · last
                reply{" "}
                {thread.lastReplyAt === undefined ? null : <Timestamp at={thread.lastReplyAt} />}
              </p>
            )}
          </button>
        )}
      </article>
    </>
  );
}
