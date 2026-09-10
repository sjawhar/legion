import type { ReactNode, RefObject } from "react";
import { Link } from "react-router-dom";

import type { Comment } from "../../api/types";
import { AskCard } from "../inbox/AskCard";
import { buildIssuePath } from "../refs/routes";
import { Unfurl } from "../refs/Unfurl";
import { Composer, type ComposerAnchor, type ComposerKind } from "./Composer";
import { type MarginItem, type MarginItemAction, marginItemId } from "./useMarginItems";

export interface MarginComposer {
  anchor: ComposerAnchor;
  kind: ComposerKind;
  replyTo?: string;
}

interface CommentsTabProps {
  artifactSlug: string;
  asksPending: boolean;
  commentsPending: boolean;
  composer: MarginComposer | undefined;
  hoveredItemId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  items: MarginItem[];
  list: RefObject<HTMLDivElement | null>;
  onAction: (id: string, action: MarginItemAction) => void;
  onCloseComposer: () => void;
  onReply: (comment: Comment) => void;
  selectedItemId: string | undefined;
}

function CommentCard({
  artifactSlug,
  comment,
  depth,
  onAction,
  onReply,
  selected,
}: {
  artifactSlug: string;
  comment: Comment;
  depth: number;
  onAction: (id: string, action: MarginItemAction) => void;
  onReply: (comment: Comment) => void;
  selected: boolean;
}): ReactNode {
  const suggestion = comment.suggestion;
  const anchor = comment.anchor;
  return (
    <article
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        selected ? "border-sky-500 bg-sky-50" : "border-slate-200 bg-white"
      }`}
      data-margin-item={comment.id}
      data-testid={`margin-comment-${comment.id}`}
      style={{ marginLeft: `${depth * 12}px` }}
    >
      {anchor === null ? null : (
        <blockquote className="mb-2 border-l-2 border-sky-400 pl-2 text-slate-600">
          {anchor.quote}
        </blockquote>
      )}
      {anchor?.orphaned ? (
        <p className="mb-2 text-xs font-medium text-amber-800">
          Text changed.{" "}
          <Link
            className="underline"
            to={`${buildIssuePath({
              key: comment.issue_key,
              kind: "artifact",
              slug: artifactSlug,
              version: anchor.version,
            })}&from=${anchor.from}&to=${anchor.to}`}
          >
            View original text
          </Link>
        </p>
      ) : null}
      {suggestion === null ? <p className="whitespace-pre-wrap">{comment.body}</p> : null}
      {suggestion !== null && anchor !== null ? (
        <div className="space-y-1 font-mono text-xs">
          <del className="block rounded bg-rose-50 px-2 py-1 text-rose-800">{anchor.quote}</del>
          <ins className="block rounded bg-emerald-50 px-2 py-1 text-emerald-800">
            {suggestion.replace_with}
          </ins>
          {comment.body === "Suggested replacement." ? null : (
            <p className="whitespace-pre-wrap font-sans text-slate-700">{comment.body}</p>
          )}
        </div>
      ) : null}
      <Unfurl body={comment.body} />
      <p className="mt-2 text-xs text-slate-500">
        {comment.author.id} · {new Date(comment.created_at).toLocaleString()}
      </p>
      <div className="mt-2 flex flex-wrap gap-3 text-sm">
        {suggestion !== null && suggestion.accepted === null && !comment.resolved ? (
          <>
            <button
              className="font-medium text-emerald-700 hover:text-emerald-900"
              onClick={() => onAction(comment.id, "accept")}
              type="button"
            >
              Accept
            </button>
            <button
              className="font-medium text-rose-700 hover:text-rose-900"
              onClick={() => onAction(comment.id, "reject")}
              type="button"
            >
              Reject
            </button>
          </>
        ) : suggestion === null && !comment.resolved ? (
          <button
            className="font-medium text-sky-700 hover:text-sky-900"
            onClick={() => onAction(comment.id, "resolve")}
            type="button"
          >
            Resolve
          </button>
        ) : (
          <span className="font-medium text-slate-500">
            {suggestion?.accepted === true
              ? "Accepted"
              : suggestion?.accepted === false
                ? "Rejected"
                : "Resolved"}
          </span>
        )}
        {anchor === null ? null : (
          <button
            className="font-medium text-sky-700 hover:text-sky-900"
            onClick={() => onReply(comment)}
            type="button"
          >
            Reply
          </button>
        )}
      </div>
    </article>
  );
}

export function CommentsTab({
  artifactSlug,
  asksPending,
  commentsPending,
  composer,
  hoveredItemId,
  isClosed,
  issueKey,
  items,
  list,
  onAction,
  onCloseComposer,
  onReply,
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
        {items.map((item) => {
          const id = marginItemId(item);
          const active = selectedItemId === id || hoveredItemId === id;
          return (
            <div key={id}>
              {item.kind === "ask" ? (
                <div
                  className={active ? "rounded-xl ring-2 ring-sky-400" : undefined}
                  data-margin-item={id}
                >
                  <AskCard ask={item.ask} />
                  <Unfurl body={item.ask.question} />
                  <p className="mt-2 text-xs text-slate-500">
                    {new Date(item.ask.created_at).toLocaleString()}
                  </p>
                </div>
              ) : (
                <CommentCard
                  artifactSlug={artifactSlug}
                  comment={item.comment}
                  depth={item.depth}
                  onAction={onAction}
                  onReply={onReply}
                  selected={active}
                />
              )}
            </div>
          );
        })}
        {items.length === 0 && !asksPending && !commentsPending ? (
          <p className="text-sm text-slate-500">
            No comments, asks, or suggestions on this document.
          </p>
        ) : null}
      </section>
    </div>
  );
}
