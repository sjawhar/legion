import { type MutationKey, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { userStateQuery, whoAmIQuery } from "../../api/queries";
import type {
  Agent,
  Artifact,
  Comment,
  Message,
  MessageDelivery,
  MessageDeliveryMode,
  UserState,
} from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { useSending } from "../../hooks/useSending";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  dangerText,
  linkText,
  primaryButtonBg,
  primaryButtonHoverBg,
  secondaryButtonBorder,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { deliveryAttempts } from "../agents/attempts";
import { AskCard } from "../inbox/AskCard";
import { conversationItemId, isPinnedConversation, stateForIssue } from "../issue/pins";
import { usePinWrites } from "../issue/usePinWrites";
import { ThreadCard } from "../margin/ThreadCard";
import { type CommentActionFailure, useCommentActionQueue } from "../margin/useCommentActionQueue";
import type { Thread as CommentThread } from "../margin/useMarginItems";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, documentItemPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { Unfurl } from "../refs/Unfurl";
import { useMarkThreadRead } from "../replies/useMarkThreadRead";
import { PHONE_VIEWPORT_QUERY, useDialog, useMediaQuery } from "../shell/useDialog";
import { type DwellRef, useDwell } from "../shell/useDwell";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { Avatar } from "./Avatar";
import { type Author, resolveAuthor } from "./authors";
import type { ReplyTarget } from "./composer-model";
import {
  buildConversations,
  type CommentDeliveryAttempt,
  type Conversation,
  type ConversationFilter,
  commentDeliveries,
  focusConversation,
  type MessageConversation,
  matchesFilter,
} from "./conversation-model";
import { duplicateText, failureGuidance, isSafeRetry, MODE_LABELS, withGuidance } from "./delivery";
import {
  redirectedReplyTarget,
  useHeldReplyTo,
  useHeldSends,
  useHeldSendsUnder,
} from "./held-sends";
import { MentionComposer } from "./MentionComposer";
import { ReplyButton } from "./ReplyButton";
import { ReplyTurn, ThreadReplies, TurnActions } from "./ReplyTurn";
import { capabilitiesForTarget, TargetedMessageCard } from "./TargetedMessageCard";
import { useFollowLatest } from "./use-follow-latest";
import { useAgents } from "./useAgents";
import { threadRepliesSendKey, threadReplySendKey, usePhoneThread } from "./usePhoneThread";

interface ConversationTabProps {
  /** Every artifact on the issue by id - uploads and images as well as documents, which is
   *  what `IssuePage` has. A comment's anchor names one of them, and its link is built from it. */
  issueArtifacts: ReadonlyMap<string, Artifact>;
  focusItemId?: string;
  isClosed: boolean;
  issueKey: string;
  state: UserState | undefined;
  visible: boolean;
}

const FILTERS: readonly { readonly label: string; readonly value: ConversationFilter }[] = [
  { label: "All", value: "all" },
  { label: "Open", value: "open" },
  { label: "Closed", value: "closed" },
];

/** The live session behind a stored delivery, by the id the attempt recorded. */
function sessionTitle(sessionId: string | null, agents: readonly Agent[]): string | undefined {
  return agents.find((agent) => agent.session_id === sessionId)?.title;
}

/** The session a thread's newest attempt reached, or the target it was aimed at: what the card
 *  and its delivery lines call the recipient. */
function targetNameOf(
  deliveries: readonly MessageDelivery[],
  target: string | null,
  agents: readonly Agent[]
): string {
  const last = deliveries.at(-1);
  return (
    (last === undefined ? undefined : sessionTitle(last.session_id, agents)) ?? target ?? "agent"
  );
}

/** The mark an unread conversation carries: its newest activity is at or after the reader's
 *  watermark (`user_issue_state.last_read_seq`). */
function UnreadMark(): ReactNode {
  return (
    <span
      className={`shrink-0 rounded-full px-2 py-0.5 text-xs font-semibold ${surfaceMutedBg} ${linkText}`}
    >
      New
    </span>
  );
}

/** The delivery a reply into this thread inherits: the root's target, in the mode of the
 *  thread's most recent attempt (root or any reply), named after the session that received it. */
function threadDelivery(
  root: MessageConversation,
  agents: readonly Agent[]
): ReplyTarget["thread"] | undefined {
  if (root.kind !== "targeted-message" || root.root.target === null) {
    return undefined;
  }
  let last: MessageDelivery | undefined;
  for (const attempt of [
    ...root.root.deliveries,
    ...root.replies.flatMap((reply) => reply.deliveries),
  ]) {
    if (last === undefined || compareTimestamps(attempt.created_at, last.created_at) >= 0) {
      last = attempt;
    }
  }
  const target = root.root.target;
  return {
    delivery: last?.delivery ?? "steer",
    target,
    title: sessionTitle(last?.session_id ?? null, agents) ?? target,
  };
}

/** What the composer quotes when the reader replies to `message`. */
function replyTargetFor(
  message: Message,
  author: Author,
  root: MessageConversation,
  issueKey: string,
  agents: readonly Agent[]
): ReplyTarget {
  const thread = threadDelivery(root, agents);
  return {
    author: author.label,
    excerpt: message.body,
    id: message.id,
    parentKind: "message",
    to: buildIssuePath({ id: message.id, key: issueKey, kind: "message" }),
    ...(thread === undefined ? {} : { thread }),
  };
}

/** A reply in a thread as the Conversation renders it: the quoted parent links to that turn,
 *  a delivered reply (a human's follow-up on a targeted thread) shows its attempt and keeps the
 *  retry row while unanswered, and every reply can be replied to while the issue is open. */
function ConversationReply({
  agents,
  currentReplyId,
  isClosed,
  issueKey,
  markRef,
  onReply,
  reply,
  replyDisabled,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentReplyId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  /** The Replies page's read mark, on the thread's newest reply. */
  markRef?: DwellRef;
  onReply: (target: ReplyTarget) => void;
  reply: Message;
  replyDisabled: boolean;
  root: MessageConversation;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: MessageDeliveryMode) => api.createMessageDelivery(reply.id, delivery),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["issue-messages", issueKey] }),
  });
  const author = resolveAuthor(reply.author, titles);
  const source = { id: reply.id, kind: "message" } as const;
  const capabilities = capabilitiesForTarget(root.root.target, agents);
  const lastAttempt = reply.deliveries.at(-1);
  const targetName = targetNameOf(reply.deliveries, root.root.target, agents);
  const answer = root.replies.find(
    (candidate) => candidate.in_reply_to === reply.id && candidate.author.kind === "session"
  );
  const parent = [root.root, ...root.replies].find((node) => node.id === reply.in_reply_to);
  return (
    <ReplyTurn
      actions={<CopyRefButton route={{ id: reply.id, key: issueKey, kind: "message" }} />}
      at={reply.created_at}
      author={author}
      body={
        <>
          <MarkdownBody markdown={reply.body} source={source} />
          <Unfurl body={reply.body} source={source} />
        </>
      }
      current={reply.id === currentReplyId}
      markRef={markRef}
      delivery={
        lastAttempt === undefined
          ? undefined
          : {
              answeredBy:
                answer === undefined ? undefined : resolveAuthor(answer.author, titles).label,
              attempts: deliveryAttempts(reply.deliveries, targetName),
              retry: isClosed
                ? undefined
                : {
                    canAside: capabilities?.includes("aside") !== false,
                    canBtw: capabilities?.includes("btw") !== false,
                    canSteer: capabilities?.includes("steer") !== false,
                    onRetry: retry.mutate,
                    retrying: retry.isPending,
                  },
              targetName,
            }
      }
      onReply={
        isClosed ? undefined : () => onReply(replyTargetFor(reply, author, root, issueKey, agents))
      }
      quote={{
        author: parent === undefined ? undefined : resolveAuthor(parent.author, titles).label,
        excerpt: parent?.body ?? "",
        source: parent === undefined ? undefined : { id: parent.id, kind: "message" },
        to:
          reply.in_reply_to === null
            ? undefined
            : buildIssuePath({ id: reply.in_reply_to, key: issueKey, kind: "message" }),
      }}
      replyDisabled={replyDisabled}
      turnID={conversationItemId("message", reply.id)}
    />
  );
}

function Thread({
  agents,
  currentReplyId,
  isClosed,
  issueKey,
  onReply,
  replyDisabled,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentReplyId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onReply: (target: ReplyTarget) => void;
  replyDisabled: boolean;
  root: MessageConversation;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  // The Replies page's read mark: the thread is read once its newest reply has been on screen.
  // It watches that reply's own element, so a thread taller than the screen still clears.
  const newest = root.replies.at(-1);
  const markRef = useMarkThreadRead({
    enabled: true,
    newestShownAt: newest?.created_at,
    owner: { issue: issueKey },
    threadId: root.root.id,
  });
  if (root.replies.length === 0) return null;
  return (
    <ThreadReplies>
      {root.replies.map((reply) => (
        <ConversationReply
          agents={agents}
          currentReplyId={currentReplyId}
          isClosed={isClosed}
          issueKey={issueKey}
          key={reply.id}
          markRef={reply === newest ? markRef : undefined}
          onReply={onReply}
          reply={reply}
          replyDisabled={replyDisabled}
          root={root}
          titles={titles}
        />
      ))}
    </ThreadReplies>
  );
}

function TurnPin({
  disabled,
  onPin,
  pinned,
}: {
  disabled: boolean;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  return (
    <PinButton
      disabled={disabled}
      label={pinned ? "Unpin" : "Pin"}
      onClick={onPin}
      pinned={pinned}
      quiet
    />
  );
}

function MessageTurn({
  agents,
  author,
  current,
  currentReplyId,
  disabled,
  isClosed,
  issueKey,
  item,
  onPin,
  onReply,
  pinned,
  register,
  replyDisabled,
  titles,
}: {
  agents: readonly Agent[];
  author: Author;
  current: boolean;
  currentReplyId: string | undefined;
  disabled: boolean;
  isClosed: boolean;
  issueKey: string;
  item: Extract<Conversation, { kind: "message" }>;
  onPin: () => void;
  onReply: (target: ReplyTarget) => void;
  pinned: boolean;
  register?: DwellRef;
  replyDisabled: boolean;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const source = { id: item.root.id, kind: "message" } as const;
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`group mt-2 rounded-lg px-2 py-1 ${surfaceMutedHoverBg}`}
      data-turn={item.id}
      ref={register}
    >
      <div className="flex gap-3">
        <Avatar author={author} />
        <div className="min-w-0 flex-1">
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-semibold">{author.label}</span>
            <Timestamp at={item.root.created_at} />
            {item.unread ? <UnreadMark /> : null}
          </p>
          <MarkdownBody markdown={item.root.body} source={source} />
          <Unfurl body={item.root.body} source={source} />
        </div>
        <TurnActions>
          <CopyRefButton route={{ id: item.root.id, key: issueKey, kind: "message" }} />
          {isClosed ? null : (
            <ReplyButton
              disabled={replyDisabled}
              onClick={() => onReply(replyTargetFor(item.root, author, item, issueKey, agents))}
            />
          )}
          <TurnPin disabled={disabled} onPin={onPin} pinned={pinned} />
        </TurnActions>
      </div>
      <Thread
        agents={agents}
        currentReplyId={currentReplyId}
        isClosed={isClosed}
        issueKey={issueKey}
        onReply={onReply}
        replyDisabled={replyDisabled}
        root={item}
        titles={titles}
      />
    </li>
  );
}

function commentReplyTarget(
  comment: Comment,
  agents: readonly Agent[],
  issueKey: string
): ReplyTarget {
  const author = comment.author;
  const targets =
    author.kind === "session"
      ? [`session:${author.id}`]
      : comment.mentions.map((mention) => mention.target);
  const mentions = [...new Set(targets)].map((target) => ({
    target,
    title: target.startsWith("session:")
      ? (agents.find((agent) => `session:${agent.session_id}` === target)?.title ??
        target.slice("session:".length))
      : target.slice("role:".length),
  }));
  return {
    author: resolveAuthor(author, new Map(agents.map((agent) => [agent.session_id, agent.title])))
      .label,
    excerpt: comment.body,
    id: comment.id,
    mentions,
    parentKind: "comment",
    to: buildIssuePath({ id: comment.id, key: issueKey, kind: "comment" }),
  };
}

function CommentDeliveryList({
  agents,
  deliveries,
  disabled,
  onRetry,
  retrying,
}: {
  agents: readonly Agent[];
  deliveries: readonly CommentDeliveryAttempt[];
  disabled: boolean;
  onRetry: (delivery: CommentDeliveryAttempt) => void;
  retrying: boolean;
}): ReactNode {
  if (deliveries.length === 0) return null;
  return (
    <ul
      aria-label="Mention deliveries"
      className={`mt-2 space-y-1 text-xs ${textSecondaryOnSurface}`}
    >
      {deliveries.map((delivery) => {
        const capabilities = capabilitiesForTarget(delivery.target, agents);
        const outcome = {
          state: delivery.state,
          createdAt: delivery.created_at,
          duplicate: delivery.duplicate,
          answeredWithError: delivery.answeredWithError,
          error: delivery.error,
        };
        // The same rule the targeted-message card applies: a mention's Retry re-sends its own
        // mode under its own key, so it is offered only while `isSafeRetry` holds, and the
        // guidance beside it names that Retry or a new comment, so a closed issue, which takes
        // neither, says nothing about one.
        const safeRetry = isSafeRetry(outcome);
        const canRetry = capabilities === undefined || capabilities.includes(delivery.delivery);
        const guidance = disabled ? undefined : failureGuidance(outcome, "mention");
        return (
          <li
            className="flex flex-wrap items-center gap-x-2"
            key={`${delivery.target}:${delivery.attempt}`}
          >
            <span>
              {delivery.target} · {delivery.duplicate === true ? duplicateText : delivery.state}
              {delivery.error === null
                ? ""
                : ` · ${guidance === undefined ? delivery.error : withGuidance(delivery.error, guidance)}`}
              {delivery.error === null && guidance !== undefined ? ` · ${guidance}` : ""}
            </span>
            {disabled || !canRetry || !safeRetry ? null : (
              <button
                className={`min-h-8 font-medium ${linkText}`}
                disabled={retrying}
                onClick={() => onRetry(delivery)}
                type="button"
              >
                Retry
              </button>
            )}
            {disabled || canRetry ? null : (
              <span className={textMutedOnSurface}>
                {delivery.target} no longer supports {MODE_LABELS[delivery.delivery]}.
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function CommentTurn({
  actionFailure,
  agents,
  issueArtifacts,
  composerClassName,
  current,
  currentReplyId,
  disabled,
  expanded,
  forceExpanded = false,
  hideReplyComposer = false,
  isClosed,
  isPhone = false,
  issueKey,
  item,
  onAction,
  onPhoneThreadToggle,
  onPin,
  onReply,
  onRetryAction,
  pendingAction,
  pinned,
  register,
  replyDisabled,
  sendKey,
  setExpanded,
  viewerLogin,
}: {
  actionFailure: CommentActionFailure | undefined;
  agents: readonly Agent[];
  issueArtifacts: ConversationTabProps["issueArtifacts"];
  composerClassName?: string;
  current: boolean;
  /** The reply a deep link named, marked inside the thread. */
  currentReplyId: string | undefined;
  disabled: boolean;
  /** Whether the reader has the thread open inline. The tab owns it, so a comment whose thread is
   *  open stays in the list whoever resolves it (`shown`), with its reply and the reply's send. */
  expanded: boolean;
  forceExpanded?: boolean;
  hideReplyComposer?: boolean;
  isClosed: boolean;
  isPhone?: boolean;
  issueKey: string;
  item: Extract<Conversation, { kind: "comment" }>;
  onAction: (id: string, kind: "accept" | "reject" | "resolve" | "reopen") => void;
  onPhoneThreadToggle?: () => void;
  onPin: () => void;
  /** Starts a reply, and says whether it did: a send that is out refuses it. On a phone a comment's
   *  reply is its thread's, so the host opens the thread with it. */
  onReply: (target: ReplyTarget) => boolean;
  onRetryAction: () => void;
  pendingAction: boolean;
  pinned: boolean;
  register?: DwellRef;
  replyDisabled: boolean;
  /** The tab's send name, beneath which the thread card's own reply composer names its send
   *  (`threadReplySendKey`). */
  sendKey: MutationKey;
  setExpanded: (expanded: boolean) => void;
  viewerLogin: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const editComment = useCallback(
    async (id: string, body: string) => {
      setSavingCommentEditId(id);
      try {
        const comment = await api.editComment(id, { body });
        await queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
        return comment;
      } finally {
        setSavingCommentEditId(undefined);
      }
    },
    [issueKey, queryClient]
  );
  const replySendKey = useMemo(
    () => threadReplySendKey(sendKey, item.root.id),
    [item.root.id, sendKey]
  );
  // Collapse thread waits for the reply until its deadline, so its answer lands where it was sent;
  // past it the reader may leave, and the held-send store keeps the reply for the thread's return.
  const { sending: replySending, sendingNow: replySendingNow } = useSending(replySendKey, {
    untilDeadline: true,
  });
  const retryGuard = useSubmitGuard();
  const retry = useMutation({
    mutationFn: (delivery: CommentDeliveryAttempt) =>
      api.createCommentDelivery(delivery.comment_id, delivery.target, delivery.delivery),
    onSettled: () => retryGuard.release(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] }),
  });
  const thread: CommentThread = {
    anchor: item.root.anchor,
    key: item.root.id,
    lastReplyAt: item.replies.at(-1)?.created_at,
    replies: [...item.replies],
    resolved: item.closed,
    root: { comment: item.root, kind: "comment" },
  };
  const deliveriesFor = (commentId: string): readonly CommentDeliveryAttempt[] =>
    commentDeliveries(
      (commentId === item.root.id
        ? item.root
        : item.replies.find((reply) => reply.id === commentId)
      )?.deliveries ?? []
    );
  const anchorArtifact =
    item.root.anchor === null ? undefined : issueArtifacts.get(item.root.anchor.artifact_id);
  const artifactSlug = anchorArtifact?.slug;
  const currentExpanded = forceExpanded || expanded;
  const toggleThread = () => {
    // A thread whose own reply is out stays open, from Send's own task on, until the send's
    // deadline (`replySendingNow`'s `untilDeadline`).
    if (currentExpanded && replySendingNow()) return;
    if (forceExpanded || (isPhone && !expanded)) {
      onPhoneThreadToggle?.();
      return;
    }
    setExpanded(!expanded);
  };
  return (
    <li
      aria-current={current ? "true" : undefined}
      className="my-2"
      data-turn={item.id}
      ref={register}
    >
      {item.unread ? (
        <div className="mb-1 flex justify-end">
          <UnreadMark />
        </div>
      ) : null}
      <ThreadCard
        actionFailure={actionFailure}
        artifactSlug={artifactSlug}
        composerClassName={composerClassName}
        currentReplyId={currentReplyId}
        expanded={currentExpanded}
        hovered={false}
        hideReplyComposer={hideReplyComposer}
        isClosed={isClosed}
        editingCommentId={editingCommentId}
        savingCommentEditId={savingCommentEditId}
        onAction={onAction}
        onEdit={editComment}
        onEditingChange={setEditingCommentId}
        onRetryAction={onRetryAction}
        onToggle={toggleThread}
        owner={{ key: issueKey, kind: "issue" }}
        pendingAction={pendingAction}
        pulseOrphanBlock={false}
        replyMutationKey={replySendKey}
        showReference={false}
        renderDeliveries={(comment) => (
          <CommentDeliveryList
            agents={agents}
            deliveries={deliveriesFor(comment.id)}
            disabled={isClosed}
            onRetry={(delivery) => retryGuard.guard(() => retry.mutate(delivery))}
            retrying={retry.isPending}
          />
        )}
        thread={thread}
        viewerLogin={viewerLogin}
      />
      {item.root.anchor === null || anchorArtifact === undefined ? null : (
        <Link
          className={`ml-3 text-sm ${linkText}`}
          to={documentItemPath(anchorArtifact, { id: item.root.id, kind: "comment" })}
        >
          View in document
        </Link>
      )}
      <div className="flex justify-end gap-1">
        {forceExpanded ? null : (
          <button
            aria-expanded={currentExpanded}
            className={`min-h-8 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${linkText}`}
            disabled={currentExpanded && replySending}
            onClick={toggleThread}
            type="button"
          >
            {currentExpanded ? "Collapse thread" : "Expand thread"}
          </button>
        )}
        <CopyRefButton route={{ id: item.root.id, key: issueKey, kind: "comment" }} />
        {isClosed ? null : (
          <ReplyButton
            disabled={replyDisabled}
            onClick={() => {
              if (onReply(commentReplyTarget(item.root, agents, issueKey)) && !isPhone) {
                setExpanded(true);
              }
            }}
          />
        )}
        <TurnPin disabled={disabled} onPin={onPin} pinned={pinned} />
      </div>
    </li>
  );
}

function TargetedMessageTurn({
  agents,
  current,
  currentReplyId,
  isClosed,
  issueKey,
  item,
  onReply,
  register,
  replyDisabled,
  titles,
}: {
  agents: readonly Agent[];
  current: boolean;
  currentReplyId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  item: Extract<Conversation, { kind: "targeted-message" }>;
  onReply: (target: ReplyTarget) => void;
  register?: DwellRef;
  replyDisabled: boolean;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: MessageDeliveryMode) =>
      api.createMessageDelivery(item.root.id, delivery),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["issue-messages", issueKey] }),
  });
  const capabilities = capabilitiesForTarget(item.root.target, agents);
  const targetName = targetNameOf(item.root.deliveries, item.root.target, agents);
  const asker = resolveAuthor(item.root.author, titles);
  const source = { id: item.root.id, kind: "message" } as const;

  return (
    <TargetedMessageCard
      answeredBy={
        item.answer === undefined ? undefined : resolveAuthor(item.answer.author, titles).label
      }
      body={
        <>
          <p className={`flex flex-wrap items-center gap-x-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-semibold">{asker.label}</span>
            <Timestamp at={item.root.created_at} />
            <CopyRefButton route={{ id: item.root.id, key: issueKey, kind: "message" }} />
            {item.unread ? <UnreadMark /> : null}
          </p>
          <MarkdownBody markdown={item.root.body} source={source} />
          <Unfurl body={item.root.body} source={source} />
        </>
      }
      canAside={capabilities?.includes("aside") !== false}
      canBtw={capabilities?.includes("btw") !== false}
      canSteer={capabilities?.includes("steer") !== false}
      current={current}
      deliveries={deliveryAttempts(item.root.deliveries, targetName)}
      header={<Avatar author={asker} />}
      isClosed={isClosed}
      onReply={() => onReply(replyTargetFor(item.root, asker, item, issueKey, agents))}
      onRetry={retry.mutate}
      register={register}
      replyDisabled={replyDisabled}
      retrying={retry.isPending}
      targetName={targetName}
      thread={
        <Thread
          agents={agents}
          currentReplyId={currentReplyId}
          isClosed={isClosed}
          issueKey={issueKey}
          onReply={onReply}
          replyDisabled={replyDisabled}
          root={item}
          titles={titles}
        />
      }
      turnID={item.id}
    />
  );
}

export function ConversationTab({
  issueArtifacts,
  focusItemId,
  isClosed,
  issueKey,
  state,
  visible,
}: ConversationTabProps): ReactNode {
  const queryClient = useQueryClient();
  // Every composer this tab shows names its send with `sendKey`, or beneath it: the docked one
  // with the key itself, the phone thread's composer and a thread card's inline reply beneath it
  // (`usePhoneThread`'s `composerKey`, `threadReplySendKey`).
  // While one is out, every Reply holds, from Send's own task on (`sendingNow`, which matches a key
  // and every key beneath it): a Reply would put its prefill over the draft on its way, or swap the
  // composer that holds it for another. The request itself is frozen when Send starts
  // (`MentionComposer`), and the held-send store holds each send by its name, out or refused,
  // whatever unmounts its composer; this decides what the reader sees.
  const sendKey = useMemo<MutationKey>(() => ["conversation-composer", issueKey], [issueKey]);
  const { sending: composerSending, sendingNow } = useSending(sendKey);
  const store = useHeldSends();
  // Comments whose card's own reply the store holds, out or refused.
  const heldCardReplies = useHeldSendsUnder(threadRepliesSendKey(sendKey));
  const pins = usePinWrites(issueKey);
  const viewer = useQuery(whoAmIQuery());
  // The three reads the list is built from, each the issue's whole set: a conversation is listed
  // however old it is, which no window of the event log could promise.
  // The comments are the read the margin already holds for this issue, under the key it owns
  // (`features/margin/useMarginItems.ts`): the same request, shared rather than fetched and
  // stored a second time. The asks are not: this tab draws each ask's thread and asks the list
  // for it, where the margin and a document's decision blocks render the ask alone and read
  // `["asks", issueKey]` without one. Hence a key of its own - beneath that prefix, so every
  // `invalidateQueries({ queryKey: ["asks", key] })` already written reaches this read too.
  const asks = useQuery({
    queryKey: ["asks", issueKey, "thread"],
    queryFn: () => api.listIssueAskThreads(issueKey, "all"),
  });
  const comments = useQuery({
    queryKey: ["comments", issueKey],
    queryFn: () => api.listComments(issueKey),
  });
  const messages = useQuery({
    queryKey: ["issue-messages", issueKey],
    queryFn: () => api.listIssueMessages(issueKey),
  });
  const issueState = stateForIssue(state, issueKey);
  const lastReadSeq = issueState.last_read_seq;
  // The watermark as a moment: the first event the reader has not read. One row - the list is
  // ordered by activity, not by sequence, so the log is read for this and for the head alone.
  const unreadBoundary = useQuery({
    queryKey: ["events", issueKey, "unread-boundary", lastReadSeq],
    queryFn: () => api.getIssueEvents(issueKey, { after: lastReadSeq, limit: 1 }),
  });
  // The issue's head: what a dwell on the newest conversation marks read.
  const head = useQuery({
    queryKey: ["events", issueKey, "head"],
    queryFn: () => api.getIssueEvents(issueKey, { limit: 1, order: "desc" }),
  });
  const unreadFrom = unreadBoundary.data?.[0]?.created_at;
  const headSeq = head.data?.[0]?.seq ?? 0;
  const lastRead = useRef(lastReadSeq);
  const conversations = useMemo(
    () =>
      buildConversations({
        asks: asks.data ?? [],
        comments: comments.data ?? [],
        messages: messages.data ?? [],
        unreadFrom,
      }),
    [asks.data, comments.data, messages.data, unreadFrom]
  );
  const [filter, setFilter] = useState<ConversationFilter>("all");
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const phone = usePhoneThread({ isPhoneViewport, sendKey, sendingNow });
  // The comments whose thread the reader has open inline.
  const [openThreads, setOpenThreads] = useState<ReadonlySet<string>>(() => new Set());
  const setThreadOpen = useCallback((id: string, open: boolean) => {
    setOpenThreads((current) => {
      if (current.has(id) === open) return current;
      const next = new Set(current);
      if (open) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);
  // The conversation a deep link names, and the reply inside it when the link names one.
  const focus = useMemo(
    () =>
      focusConversation(focusItemId, {
        asks: asks.data ?? [],
        comments: comments.data ?? [],
        messages: messages.data ?? [],
      }),
    [asks.data, comments.data, focusItemId, messages.data]
  );
  const shown = useMemo(
    () =>
      // One pass over the list in the order `buildConversations` gave it, newest activity first:
      // a conversation the reader has in hand is kept wherever the filter would have dropped it -
      // its thread open inline or full-screen, the phone thread composer holding a reply to it,
      // or its card's own reply held, out or refused, for its thread to show again - and keeps
      // its place in that order rather than falling in behind the conversations the filter took.
      conversations.filter(
        (conversation) =>
          matchesFilter(conversation, filter) ||
          (conversation.kind === "comment" &&
            (openThreads.has(conversation.root.id) ||
              phone.holds(conversation.root.id) ||
              heldCardReplies.some((held) => held.request.replyTo?.id === conversation.root.id)))
      ),
    [conversations, filter, heldCardReplies, openThreads, phone.holds]
  );
  // Counted over the list as shown, which is the list the pill acts on: it jumps to the first
  // card on the page, so a count taken over the whole set would name unread conversations the
  // filter is hiding and land on a card carrying no `New` mark.
  const unreadCount = shown.filter((conversation) => conversation.unread).length;
  // A landing overrides the filter: arriving at a conversation the current bucket hides resets
  // it to All, so a link never fails to land.
  useEffect(() => {
    if (focus === undefined || filter === "all") return;
    if (shown.some((conversation) => conversation.id === focus.id)) return;
    if (!conversations.some((conversation) => conversation.id === focus.id)) return;
    setFilter("all");
  }, [conversations, filter, focus, shown]);
  const phoneThread = useMemo(
    () =>
      phone.id === undefined
        ? undefined
        : shown.find(
            (item): item is Extract<Conversation, { kind: "comment" }> =>
              item.kind === "comment" && item.root.id === phone.id
          ),
    [phone.id, shown]
  );
  const phoneThreadDialog = useDialog<HTMLElement>({
    onClose: phone.leave,
    open: phoneThread !== undefined,
  });
  const commentActions = useCommentActionQueue({
    // Resolving a comment, or accepting or rejecting its suggestion, is the reader done with its
    // thread: once the server takes it, the thread closes. A refusal leaves the thread and the
    // reply the reader was writing as they were, and a thread whose own reply is out stays open,
    // which closing would unmount.
    onSuccess: ({ id, kind }) => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      if (kind === "reopen") return;
      if (queryClient.isMutating({ mutationKey: threadReplySendKey(sendKey, id) }) > 0) return;
      setThreadOpen(id, false);
      if (phone.id === id) phone.hide();
    },
  });
  const [ownSendCount, setOwnSendCount] = useState(0);
  // The docked composer's reply. A message's Reply answers here at every width, and so does a
  // comment's above the phone layout - the same shared composer and send name
  // (`redirectedReplyTarget`'s doc comment explains why a Reply elsewhere while it holds a
  // refusal redirects onto that comment or message instead of overwriting its draft).
  const [replyTo, setReplyTo] = useHeldReplyTo(sendKey);
  const beginReply = (target: ReplyTarget): boolean => {
    if (sendingNow()) return false;
    const reply = redirectedReplyTarget(store, sendKey, target);
    setReplyTo(reply);
    return reply.id === target.id;
  };
  const phoneReplyTargetsThread =
    phoneThread !== undefined && phone.replyTo?.id === phoneThread.root.id;
  // `useFollowLatest` follows the newest card by a number that rises with its recency; the list
  // is ordered by activity, so that number is the activity's own millisecond.
  const newestShown = shown.at(0);
  const latestItemSeq = newestShown === undefined ? undefined : Date.parse(newestShown.at);
  const follow = useFollowLatest({ enabled: visible, latestItemSeq, ownSendCount });
  const { agents, titles } = useAgents(visible);
  const scrolledToFocus = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (!visible || focus === undefined || scrolledToFocus.current === focus.id) {
      return;
    }
    // Nothing to anchor on until the card the link names is on the page; the landing effect
    // resets the filter for one the current bucket hides, and `shown` brings this effect back.
    // `shown` also changes whenever a thread opens (`openThreads` is one of its inputs), which
    // is when a named reply's own row mounts.
    if (!shown.some((conversation) => conversation.id === focus.id)) {
      return;
    }
    // The reply the link names is the anchor when it names one; a card with no reply named, or
    // one whose reply is inside a collapsed thread, anchors on the card itself. Only the anchor
    // of the kind the link named ends the search: the effect below opens the named comment's
    // thread a commit later, so the first pass over a reply link finds the card alone, scrolls
    // there, and tries again - otherwise a marked reply far down a long thread would be left
    // below the fold.
    const reply =
      focus.replyId === undefined
        ? null
        : document.querySelector<HTMLElement>(`[data-reply][aria-current="true"]`);
    const anchor = reply ?? document.querySelector<HTMLElement>(`[data-turn="${focus.id}"]`);
    if (anchor !== null) {
      anchor.scrollIntoView({ block: "center" });
      if (focus.replyId === undefined || reply !== null) {
        scrolledToFocus.current = focus.id;
      }
    }
  }, [focus, shown, visible]);

  // A link to a comment (`/issues/<key>/comments/<id>`, from a Replies row, the Inbox, search or
  // a copied reference) opens that comment's thread as it lands, so its replies are on screen:
  // inline at a desktop width, the thread dialog on a phone. Once per link; the reader closes it
  // as any thread.
  const openedFocusThread = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!visible || focus === undefined || openedFocusThread.current === focus.id) return;
    const root = shown.find(
      (item): item is Extract<Conversation, { kind: "comment" }> =>
        item.kind === "comment" && item.id === focus.id
    );
    if (root === undefined) return;
    openedFocusThread.current = focus.id;
    if (isPhoneViewport) phone.open(root.root.id);
    else setThreadOpen(root.root.id, true);
  }, [focus, isPhoneViewport, phone.open, setThreadOpen, shown, visible]);

  useEffect(() => {
    lastRead.current = lastReadSeq;
  }, [lastReadSeq]);

  // The issue's read mark, and the only writer of it: the newest conversation three-quarters on
  // screen for a second reads the issue through its head. A single watermark cannot say "I read
  // the third conversation only", so a dwell lower down writes nothing, and under-marking never
  // hides an unread conversation. A write the server refuses re-reads the stored mark and dwells
  // again while the card stays on screen (`useDwell` re-arms on the rejection this rethrows).
  const newestId = conversations[0]?.id;
  const registerNewest = useDwell({
    enabled: visible && newestId !== undefined && headSeq > 0,
    onDwell: async () => {
      if (headSeq <= lastRead.current) return;
      lastRead.current = headSeq;
      try {
        const next = await api.putIssueState(issueKey, { last_read_seq: headSeq });
        queryClient.setQueryData<UserState>(["user-state"], (current) => ({
          ...current,
          [issueKey]: next,
        }));
      } catch (error) {
        const current = await queryClient.fetchQuery(userStateQuery()).catch(() => undefined);
        lastRead.current = stateForIssue(current, issueKey).last_read_seq;
        throw error;
      }
    },
    resetKey: `${issueKey} ${newestId ?? ""} ${headSeq}`,
    shouldDwell: () => headSeq > lastRead.current,
  });

  const pinConversation = (id: string) =>
    pins.updatePins((dismissed) => ({
      id,
      op: isPinnedConversation(dismissed, id) ? "unpin" : "pin",
    }));

  if (asks.isPending || comments.isPending || messages.isPending) {
    return <p className={textMutedOnCanvas}>Loading conversation…</p>;
  }
  if (asks.isError || comments.isError || messages.isError) {
    return <p className={dangerText}>Could not load the conversation.</p>;
  }

  // The reader's card keeps its viewport position across live updates - a reply arriving while
  // they browse, a pin reflow, the failed-operations banner, a card moving bucket - except while
  // they follow the newest conversation, where an arrival simply appears in place.
  return (
    <ViewportAnchor
      as="section"
      className="flex min-h-[60dvh] flex-col gap-3 pb-56 sm:pb-16 xl:pb-0"
      enabled={() => !follow.pinnedToTop()}
      item="data-turn"
      label="Conversation"
    >
      {pins.banner}
      {/* Mounted, and hidden, while a phone thread covers it: its draft, a send it has out, that
          send's refusal and its reply are all still there on Back. Mounted on a closed issue too,
          where it shows only a send of its own still out or its refusal (`closed`), so an issue
          closed and reopened under a send leaves that send's composer where it was. `contents`,
          so the composer's own box stays the one that sticks. */}
      <div className="contents" hidden={phoneThread !== undefined}>
        <MentionComposer
          closed={isClosed}
          docked
          mutationKey={sendKey}
          onCancelReply={() => setReplyTo(null)}
          onClose={() => setReplyTo(null)}
          onSent={() => {
            setReplyTo(null);
            setOwnSendCount((count) => count + 1);
          }}
          owner={{ issueKey, kind: "issue" }}
          replyTo={replyTo}
        />
      </div>
      {/* All / Open / Closed, the one control over the list. It starts on All every visit: a
          reader who filtered yesterday still meets the whole conversation today. */}
      <div className="flex items-center justify-end">
        <fieldset
          aria-label="Conversations"
          className={`flex rounded-md border ${secondaryButtonBorder}`}
        >
          {FILTERS.map(({ label, value }) => (
            <button
              aria-pressed={filter === value}
              className={`min-h-11 px-3 text-sm font-medium first:rounded-l-md last:rounded-r-md ${
                filter === value
                  ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                  : textSecondaryOnCanvas
              }`}
              key={value}
              onClick={() => setFilter(value)}
              type="button"
            >
              {label}
            </button>
          ))}
        </fieldset>
      </div>
      {/* The pill takes an edge the docked composer leaves free, so it never covers the
          composer's controls. Below `sm` the composer sits at the foot of the screen and grows
          upward (a refusal's Retry or Discard draft with it), so the pill takes the top: it sits
          where the cards begin, and sticks at the top of the screen once they scroll under it, so
          it never rises over what is above them - the issue's header, its tabs and the filter.
          Its box has no height, and `-mb-3` takes back the section's gap, so it moves nothing. From
          `sm` the composer sticks to the top and the pill floats at the foot. A thread opened
          full-screen covers the Conversation the pill acts on, and its own composer sits at the
          foot, so the pill waits for Back. */}
      {visible && phoneThread === undefined && !follow.atTop && shown.length > 0 ? (
        <div className="sticky top-4 z-20 -mb-3 flex h-0 items-start justify-center sm:contents">
          <button
            className={`min-h-11 rounded-full px-4 text-sm font-semibold whitespace-nowrap shadow-lg sm:fixed sm:right-6 sm:bottom-36 sm:z-20 xl:bottom-24 ${primaryButtonBg} ${primaryButtonHoverBg}`}
            data-testid="jump-to-latest"
            onClick={follow.jumpToLatest}
            type="button"
          >
            Jump to latest{unreadCount === 0 ? "" : ` · ${unreadCount} new`}
          </button>
        </div>
      ) : null}
      <ol
        aria-hidden={phoneThread === undefined ? undefined : true}
        aria-label="Conversation turns"
        className="flex flex-col gap-1"
        inert={phoneThread === undefined ? undefined : true}
      >
        {shown.map((item) => {
          const current = item.id === focus?.id;
          const currentReplyId = current ? focus?.replyId : undefined;
          const pinned = isPinnedConversation(issueState.dismissed, item.id);
          const register = item.id === newestId ? registerNewest : undefined;

          if (item.kind === "targeted-message") {
            return (
              <TargetedMessageTurn
                agents={agents}
                current={current}
                currentReplyId={currentReplyId}
                isClosed={isClosed}
                issueKey={issueKey}
                item={item}
                key={item.id}
                onReply={beginReply}
                register={register}
                replyDisabled={composerSending}
                titles={titles}
              />
            );
          }
          if (item.kind === "message") {
            return (
              <MessageTurn
                agents={agents}
                author={resolveAuthor(item.root.author, titles)}
                issueKey={issueKey}
                current={current}
                currentReplyId={currentReplyId}
                disabled={pins.blocked}
                isClosed={isClosed}
                item={item}
                key={item.id}
                onPin={() => pinConversation(item.id)}
                onReply={beginReply}
                pinned={pinned}
                register={register}
                replyDisabled={composerSending}
                titles={titles}
              />
            );
          }
          if (item.kind === "comment") {
            return (
              <CommentTurn
                actionFailure={
                  commentActions.actionFailure?.id === item.root.id
                    ? commentActions.actionFailure
                    : undefined
                }
                hideReplyComposer={replyTo?.parentKind === "comment" && replyTo.id === item.root.id}
                agents={agents}
                issueArtifacts={issueArtifacts}
                isPhone={isPhoneViewport}
                onPhoneThreadToggle={() => phone.open(item.root.id)}
                onAction={(id, kind) => commentActions.mutateItem({ id, kind })}
                current={current}
                currentReplyId={currentReplyId}
                disabled={pins.blocked}
                expanded={openThreads.has(item.root.id)}
                onRetryAction={commentActions.retryItem}
                pendingAction={commentActions.pendingActionIds.has(item.root.id)}
                isClosed={isClosed}
                issueKey={issueKey}
                item={item}
                key={item.id}
                onPin={() => pinConversation(item.id)}
                onReply={isPhoneViewport ? phone.beginReply : beginReply}
                pinned={pinned}
                register={register}
                replyDisabled={composerSending}
                sendKey={sendKey}
                setExpanded={(open) => setThreadOpen(item.root.id, open)}
                viewerLogin={viewer.data?.login ?? ""}
              />
            );
          }
          return (
            <li
              aria-current={current ? "true" : undefined}
              className="my-2 flex gap-3"
              data-turn={item.id}
              key={item.id}
              ref={register}
            >
              <div className="min-w-0 flex-1">
                {item.unread ? (
                  <div className="mb-1 flex justify-end">
                    <UnreadMark />
                  </div>
                ) : null}
                {/* The margin shows this same open ask beside the document, so a full
                    two-row composer here is the question, its options and its answer
                    controls twice on one screen. The list is the record: it carries the
                    compact density, whose options answer in one click and whose note field
                    stays behind its own disclosure. The row's own thread seeds the card, so a
                    page of asks makes no per-ask thread request. */}
                <AskCard
                  ask={item.ask}
                  currentReplyId={currentReplyId}
                  initialThread={{ ask: item.ask, ...item.ask.thread }}
                  initialThreadUpdatedAt={asks.dataUpdatedAt}
                  markRead
                  thread="collapsed"
                  variant="compact"
                />
              </div>
              <TurnPin
                disabled={pins.blocked}
                onPin={() => pinConversation(item.id)}
                pinned={pinned}
              />
            </li>
          );
        })}
      </ol>
      {/* The thread view stays mounted, hidden, while its reply composer holds a reply: Back, or
          another comment's thread, leaves that composer - its draft, a send it has out and the
          send's refusal - where reopening the comment's thread finds it. */}
      {phoneThread === undefined && phone.replyTo === null ? null : (
        <section
          aria-label="Thread"
          aria-modal="true"
          className={`fixed inset-0 z-20 flex flex-col ${card}`}
          hidden={phoneThread === undefined}
          ref={phoneThreadDialog.containerRef}
          role="dialog"
        >
          <header className={`flex items-center border-b px-4 py-3 ${borderDefault}`}>
            <button
              className={`min-h-11 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${textPrimaryOnSurface}`}
              disabled={phone.cardReplySending}
              onClick={phone.leave}
              type="button"
            >
              Back
            </button>
          </header>
          {phoneThread === undefined ? null : (
            <ol className="min-h-0 flex-1 overflow-y-auto px-4 pb-32">
              <CommentTurn
                actionFailure={
                  commentActions.actionFailure?.id === phoneThread.root.id
                    ? commentActions.actionFailure
                    : undefined
                }
                agents={agents}
                issueArtifacts={issueArtifacts}
                composerClassName={`fixed inset-x-0 bottom-0 z-20 border-t px-4 pt-4 pb-2 ${card} ${borderDefault}`}
                current={phoneThread.id === focus?.id}
                currentReplyId={phoneThread.id === focus?.id ? focus?.replyId : undefined}
                viewerLogin={viewer.data?.login ?? ""}
                disabled={pins.blocked}
                expanded={openThreads.has(phoneThread.root.id)}
                forceExpanded
                hideReplyComposer={phoneReplyTargetsThread}
                isClosed={isClosed}
                issueKey={issueKey}
                item={phoneThread}
                isPhone
                key={phoneThread.id}
                onAction={(id, kind) => commentActions.mutateItem({ id, kind })}
                onPhoneThreadToggle={phone.leave}
                onRetryAction={commentActions.retryItem}
                pendingAction={commentActions.pendingActionIds.has(phoneThread.root.id)}
                onPin={() => pinConversation(phoneThread.id)}
                onReply={phone.beginReply}
                pinned={isPinnedConversation(issueState.dismissed, phoneThread.id)}
                replyDisabled={composerSending}
                sendKey={sendKey}
                setExpanded={(open) => setThreadOpen(phoneThread.root.id, open)}
              />
            </ol>
          )}
          {phone.replyTo === null ? null : (
            <div hidden={!phoneReplyTargetsThread}>
              <MentionComposer
                closed={isClosed}
                frame={`fixed inset-x-0 bottom-0 z-20 border-t px-4 pt-4 pb-2 ${card} ${borderDefault}`}
                mutationKey={phone.composerKey}
                onCancelReply={phone.endReply}
                onClose={phone.endReply}
                onSent={() => {
                  phone.endReply();
                  setOwnSendCount((count) => count + 1);
                }}
                owner={{ issueKey, kind: "issue" }}
                replyTo={phone.replyTo}
              />
            </div>
          )}
        </section>
      )}
      {shown.length === 0 ? <p className={textMutedOnCanvas}>No conversations yet.</p> : null}
    </ViewportAnchor>
  );
}
