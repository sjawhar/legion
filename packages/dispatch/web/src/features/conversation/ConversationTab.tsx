import {
  replaceEqualDeep,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { userStateQuery, whoAmIQuery } from "../../api/queries";
import { type EventPages, mergeEventPages } from "../../api/sse";
import type { Agent, Artifact, Event, UserIssueState, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerText,
  linkText,
  newDividerLine,
  primaryButtonBg,
  primaryButtonHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { ActivityLine } from "../issue/ActivityLine";
import { EventBody } from "../issue/EventBody";
import { eventItemId, isPinnedEvent, stateForIssue } from "../issue/pins";
import {
  applyPinStateOperation,
  issueStateTransport,
  type PinStateOperation,
  sharedIssueStateWrites,
} from "../issue/state-write-queue";
import { ThreadCard } from "../margin/ThreadCard";
import { useCommentActionQueue } from "../margin/useCommentActionQueue";
import type { Thread as CommentThread } from "../margin/useMarginItems";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, documentItemPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { PHONE_VIEWPORT_QUERY, useDialog, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { Avatar } from "./Avatar";
import { type Author, resolveAuthor } from "./authors";
import {
  buildConversationItems,
  type CommentDeliveryAttempt,
  type CommentEvent,
  type ConversationItem,
  commentMentions,
  countRetractedAsks,
  dateKey,
  type MessageDeliveryEvent,
  type MessageEvent,
  type ThreadReply,
  visibleConversationItems,
} from "./conversation-model";
import { MentionComposer, type ReplyTarget } from "./MentionComposer";
import { ReplyButton } from "./ReplyButton";
import { firstLine, ReplyQuote, replyQuoteText } from "./ReplyQuote";
import { ReplyTurn, ThreadReplies, TurnActions } from "./ReplyTurn";
import {
  capabilitiesForTarget,
  type TargetedMessageAttempt,
  TargetedMessageCard,
} from "./TargetedMessageCard";
import { useFollowLatest } from "./use-follow-latest";
import { useShowActivity, useShowRetracted } from "./use-show-activity";
import { useAgents } from "./useAgents";

interface FailedStateOperations {
  authoritativeState: UserIssueState | undefined;
  issueKey: string;
  operations: PinStateOperation[];
}

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

function eventItems(data: { pages: Event[][] } | undefined): Event[] {
  return data?.pages.flat() ?? [];
}

/** A message's delivery attempts as `TargetedMessageCard` shows them, each named after the
 *  session it was aimed at. */
function attemptsOf(deliveries: readonly MessageDeliveryEvent[]): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    attempt: attempt.payload.attempt,
    createdAt: attempt.created_at,
    delivery: attempt.payload.delivery,
    error: attempt.payload.error,
    state: attempt.payload.state,
    targetName: attempt.payload.title,
  }));
}

const maxFocusPageLoads = 10;

type ThreadRoot = Extract<ConversationItem, { kind: "message" | "targeted-message" }>;

/** The delivery a reply into this thread inherits: the root's target, in the mode of the
 *  thread's most recent attempt (root or any reply), named after the session that received it. */
function threadDelivery(
  root: ThreadRoot,
  agents: readonly Agent[]
): ReplyTarget["thread"] | undefined {
  if (root.kind !== "targeted-message" || root.event.payload.target === null) {
    return undefined;
  }
  let last: MessageDeliveryEvent | undefined;
  for (const attempt of [
    ...root.deliveries,
    ...root.replies.flatMap((reply) => reply.deliveries),
  ]) {
    if (last === undefined || attempt.seq > last.seq) last = attempt;
  }
  const target = root.event.payload.target;
  const session = agents.find((agent) => agent.session_id === last?.payload.session_id);
  return {
    delivery: last?.payload.delivery ?? "steer",
    target,
    title: last?.payload.title || session?.title || target,
  };
}

/** What the composer quotes when the reader replies to `node`. */
function replyTargetFor(
  node: { event: MessageEvent },
  author: Author,
  root: ThreadRoot,
  issueKey: string,
  agents: readonly Agent[]
): ReplyTarget {
  const thread = threadDelivery(root, agents);
  return {
    author: author.label,
    excerpt: firstLine(node.event.payload.body),
    id: node.event.payload.id,
    parentKind: "message",
    to: buildIssuePath({ id: node.event.payload.id, key: issueKey, kind: "message" }),
    ...(thread === undefined ? {} : { thread }),
  };
}

/** A reply in a thread as the Conversation renders it: the quoted parent links to that turn,
 *  a delivered reply (a human's follow-up on a targeted thread) shows its attempt and keeps the
 *  retry row while unanswered, and every reply can be replied to while the issue is open. */
function ConversationReply({
  agents,
  currentTurnId,
  isClosed,
  issueKey,
  onReply,
  reply,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onReply: (target: ReplyTarget) => void;
  reply: ThreadReply;
  root: ThreadRoot;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") =>
      api.createMessageDelivery(reply.event.payload.id, delivery),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["events", issueKey] }),
  });
  const author = resolveAuthor(reply.author, titles);
  const parentId = reply.event.payload.in_reply_to;
  const lastAttempt = reply.deliveries.at(-1);
  const target = agents.find((agent) => agent.session_id === lastAttempt?.payload.session_id);
  const targetName = lastAttempt?.payload.title || target?.title || "agent";
  const answer = root.replies.find(
    (candidate) =>
      candidate.event.payload.in_reply_to === reply.event.payload.id &&
      candidate.author.kind === "session"
  );
  const parent = [root, ...root.replies].find((node) => node.event.payload.id === parentId);
  return (
    <ReplyTurn
      actions={
        <CopyRefButton route={{ id: reply.event.payload.id, key: issueKey, kind: "message" }} />
      }
      at={reply.at}
      author={author}
      body={<EventBody event={reply.event} />}
      current={reply.id === currentTurnId}
      delivery={
        lastAttempt === undefined
          ? undefined
          : {
              answeredBy:
                answer === undefined ? undefined : resolveAuthor(answer.author, titles).label,
              attempts: attemptsOf(reply.deliveries),
              retry: isClosed
                ? undefined
                : {
                    canBtw:
                      capabilitiesForTarget(root.event.payload.target, agents)?.includes("btw") !==
                      false,
                    canSteer:
                      capabilitiesForTarget(root.event.payload.target, agents)?.includes(
                        "steer"
                      ) !== false,
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
        text: replyQuoteText(
          parent === undefined ? undefined : resolveAuthor(parent.author, titles).label,
          parent === undefined
            ? (reply.event.payload.reply_body ?? "")
            : firstLine(parent.event.payload.body)
        ),
        to:
          parentId === null || parentId === undefined
            ? undefined
            : buildIssuePath({ id: parentId, key: issueKey, kind: "message" }),
      }}
      turnID={reply.id}
    />
  );
}

function Thread({
  agents,
  currentTurnId,
  isClosed,
  issueKey,
  onReply,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onReply: (target: ReplyTarget) => void;
  root: ThreadRoot;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  if (root.replies.length === 0) return null;
  return (
    <ThreadReplies>
      {root.replies.map((reply) => (
        <ConversationReply
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          key={reply.id}
          onReply={onReply}
          reply={reply}
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
  currentTurnId,
  disabled,
  isClosed,
  issueKey,
  item,
  onPin,
  onReply,
  pinned,
  register,
  titles,
}: {
  agents: readonly Agent[];
  author: Author;
  current: boolean;
  currentTurnId: string | undefined;
  disabled: boolean;
  isClosed: boolean;
  issueKey: string;
  item: Extract<ConversationItem, { kind: "message" }>;
  onPin: () => void;
  onReply: (target: ReplyTarget) => void;
  pinned: boolean;
  register: (element: HTMLElement | null) => void;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const replyTo = item.kind === "message" ? item.event.payload.in_reply_to : null;
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`group rounded-lg px-2 ${item.continued ? "py-0.5" : "mt-2 py-1"} ${surfaceMutedHoverBg}`}
      data-continued={String(item.continued)}
      data-event-seq={item.lastSeq}
      data-turn={item.id}
      ref={register}
    >
      <div className="flex gap-3">
        {item.continued ? <span className="w-8 shrink-0" /> : <Avatar author={author} />}
        <div className="min-w-0 flex-1">
          {item.continued ? null : (
            <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-semibold">{author.label}</span>
              <Timestamp at={item.at} />
            </p>
          )}
          {replyTo === null || replyTo === undefined ? null : (
            <ReplyQuote
              className="mb-1"
              to={buildIssuePath({ id: replyTo, key: issueKey, kind: "message" })}
            >
              {replyQuoteText(
                undefined,
                item.kind === "message" ? (item.event.payload.reply_body ?? "") : ""
              )}
            </ReplyQuote>
          )}
          <EventBody event={item.event} />
        </div>
        <TurnActions>
          <CopyRefButton route={{ id: item.event.payload.id, key: issueKey, kind: item.kind }} />
          {item.kind === "message" && !isClosed ? (
            <ReplyButton
              onClick={() => onReply(replyTargetFor(item, author, item, issueKey, agents))}
            />
          ) : null}
          <TurnPin disabled={disabled} onPin={onPin} pinned={pinned} />
        </TurnActions>
      </div>
      {item.kind === "message" ? (
        <Thread
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          onReply={onReply}
          root={item}
          titles={titles}
        />
      ) : null}
    </li>
  );
}
function commentReplyTarget(
  event: CommentEvent,
  agents: readonly Agent[],
  issueKey: string
): ReplyTarget {
  const author = event.payload.author;
  const targets =
    author.kind === "session"
      ? [`session:${author.id}`]
      : commentMentions(event.payload).map((mention) => mention.target);
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
    excerpt: firstLine(event.payload.body),
    id: event.payload.id,
    mentions,
    parentKind: "comment",
    to: buildIssuePath({ id: event.payload.id, key: issueKey, kind: "comment" }),
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
        const canRetry = capabilities === undefined || capabilities.includes(delivery.delivery);
        return (
          <li
            className="flex flex-wrap items-center gap-x-2"
            key={`${delivery.target}:${delivery.attempt}`}
          >
            <span>
              {delivery.target} · {delivery.state}
              {delivery.error === null ? "" : ` · ${delivery.error}`}
            </span>
            {disabled || !canRetry ? null : (
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
                {delivery.target} no longer supports{" "}
                {delivery.delivery === "btw"
                  ? "BTW"
                  : delivery.delivery === "aside"
                    ? "Aside"
                    : "normal delivery"}
                .
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}
function CommentTurn({
  actionError,
  agents,
  issueArtifacts,
  composerClassName,
  current,
  disabled,
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
  viewerLogin,
}: {
  actionError: boolean;
  agents: readonly Agent[];
  issueArtifacts: ConversationTabProps["issueArtifacts"];
  composerClassName?: string;
  current: boolean;
  disabled: boolean;
  forceExpanded?: boolean;
  hideReplyComposer?: boolean;
  isClosed: boolean;
  isPhone?: boolean;
  issueKey: string;
  item: Extract<ConversationItem, { kind: "comment" }>;
  onAction: (id: string, kind: "accept" | "reject" | "resolve" | "reopen") => void;
  onPhoneThreadToggle?: () => void;
  onPin: () => void;
  onReply: (target: ReplyTarget) => void;
  onRetryAction: () => void;
  pendingAction: boolean;
  pinned: boolean;
  register?: (element: HTMLElement | null) => void;
  viewerLogin: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const [expanded, setExpanded] = useState(false);
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const editComment = useCallback(
    async (id: string, body: string) => {
      setSavingCommentEditId(id);
      try {
        const comment = await api.editComment(id, { body });
        await queryClient.invalidateQueries({ queryKey: ["events", issueKey] });
        return comment;
      } finally {
        setSavingCommentEditId(undefined);
      }
    },
    [issueKey, queryClient]
  );
  const retryGuard = useSubmitGuard();
  const retry = useMutation({
    mutationFn: (delivery: CommentDeliveryAttempt) =>
      api.createCommentDelivery(delivery.comment_id, delivery.target, delivery.delivery),
    onSettled: () => retryGuard.release(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["events", issueKey] }),
  });
  const thread: CommentThread = {
    anchor: item.event.payload.anchor,
    key: item.event.payload.id,
    lastReplyAt: item.replies.at(-1)?.event.payload.created_at,
    replies: item.replies.map((reply) => reply.event.payload),
    resolved:
      item.event.payload.resolved ||
      (item.event.payload.suggestion !== null && item.event.payload.suggestion.accepted !== null),
    root: { comment: item.event.payload, kind: "comment" },
  };
  const deliveriesFor = (commentId: string): readonly CommentDeliveryAttempt[] =>
    commentId === item.event.payload.id
      ? item.deliveries
      : (item.replies.find((reply) => reply.event.payload.id === commentId)?.deliveries ?? []);
  const anchorArtifact =
    item.event.payload.anchor === null
      ? undefined
      : issueArtifacts.get(item.event.payload.anchor.artifact_id);
  const artifactSlug = anchorArtifact?.slug;
  const currentExpanded = forceExpanded || expanded;
  const toggleThread = () => {
    if (forceExpanded || (isPhone && !expanded)) {
      onPhoneThreadToggle?.();
      return;
    }
    setExpanded((value) => !value);
  };
  return (
    <li
      aria-current={current ? "true" : undefined}
      className="my-2"
      data-event-seq={item.lastSeq}
      data-turn={item.id}
      ref={register}
    >
      <ThreadCard
        actionError={actionError}
        artifactSlug={artifactSlug}
        composerClassName={composerClassName}
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
      {item.event.payload.anchor === null || anchorArtifact === undefined ? null : (
        <Link
          className={`ml-3 text-sm ${linkText}`}
          to={documentItemPath(anchorArtifact, {
            id: item.event.payload.id,
            kind: "comment",
          })}
        >
          View in document
        </Link>
      )}
      <div className="flex justify-end gap-1">
        {forceExpanded ? null : (
          <button
            aria-expanded={currentExpanded}
            className={`min-h-8 font-medium ${linkText}`}
            onClick={toggleThread}
            type="button"
          >
            {currentExpanded ? "Collapse thread" : "Expand thread"}
          </button>
        )}
        <CopyRefButton route={{ id: item.event.payload.id, key: issueKey, kind: "comment" }} />
        {isClosed ? null : (
          <ReplyButton
            onClick={() => {
              if (isPhone) {
                onPhoneThreadToggle?.();
              } else {
                setExpanded(true);
              }
              onReply(commentReplyTarget(item.event, agents, issueKey));
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
  currentTurnId,
  isClosed,
  issueKey,
  item,
  onReply,
  register,
  titles,
}: {
  agents: readonly Agent[];
  current: boolean;
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  item: Extract<ConversationItem, { kind: "targeted-message" }>;
  onReply: (target: ReplyTarget) => void;
  register: (element: HTMLElement | null) => void;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") =>
      api.createMessageDelivery(item.event.payload.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: ["events", item.event.issue_key] }),
  });
  const delivery = item.deliveries.at(-1);
  const target =
    delivery === undefined
      ? undefined
      : agents.find((agent) => agent.session_id === delivery.payload.session_id);
  const capabilities = capabilitiesForTarget(item.event.payload.target, agents);
  const targetName =
    delivery?.payload.title || target?.title || item.event.payload.target || "agent";
  const asker = resolveAuthor(item.author, titles);

  return (
    <TargetedMessageCard
      answeredBy={
        item.answer === undefined ? undefined : resolveAuthor(item.answer.author, titles).label
      }
      body={
        <>
          <p className={`flex flex-wrap items-center gap-x-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-semibold">{asker.label}</span>
            <Timestamp at={item.at} />
            <CopyRefButton route={{ id: item.event.payload.id, key: issueKey, kind: "message" }} />
          </p>
          <EventBody event={item.event} />
        </>
      }
      canBtw={capabilities?.includes("btw") !== false}
      canSteer={capabilities?.includes("steer") !== false}
      current={current}
      deliveries={attemptsOf(item.deliveries)}
      header={<Avatar author={asker} />}
      isClosed={isClosed}
      lastSeq={item.lastSeq}
      onReply={() => onReply(replyTargetFor(item, asker, item, issueKey, agents))}
      onRetry={retry.mutate}
      register={register}
      retrying={retry.isPending}
      targetName={targetName}
      thread={
        <Thread
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          onReply={onReply}
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
  const [failedOps, setFailedOps] = useState<FailedStateOperations>();
  const viewer = useQuery(whoAmIQuery());
  const [retryingFailedOps, setRetryingFailedOps] = useState(false);
  const log = useInfiniteQuery({
    initialPageParam: null as number | null,
    queryKey: ["events", issueKey],
    queryFn: ({ pageParam }) =>
      api.getIssueEvents(
        issueKey,
        pageParam === null ? { limit: 200, order: "desc" } : { before: pageParam, limit: 200 }
      ),
    // A page can hold more than the server's 200 once streamed turns sit above a full response
    // (mergeEventPages, prependEventToLog); it is still a full page with older turns beneath it.
    getNextPageParam: (page) => (page.length >= 200 ? page.at(-1)?.seq : undefined),
    // The stream (api/sse.ts) prepends turns as they are published; a refetch whose read predates
    // one of them must not take it back.
    structuralSharing: (current, incoming) =>
      replaceEqualDeep(
        current,
        mergeEventPages(current as EventPages | undefined, incoming as EventPages)
      ),
  });
  const issueState = stateForIssue(state, issueKey);
  const lastRead = useRef(issueState.last_read_seq);
  const observed = useRef(new Map<Element, number>());
  const timers = useRef(new Map<Element, number>());
  const events = useMemo(() => eventItems(log.data), [log.data]);
  const today = dateKey(new Date().toISOString());
  // A claim event's activity line names a session, so it reads the registry like every other
  // author line; nothing is fetched for a log with no claim in it.
  const { titles: activityTitles } = useAgents(
    events.some((event) => event.type === "issue.claimed" || event.type === "issue.released")
  );
  const items = useMemo(
    () =>
      buildConversationItems({
        events,
        lastReadSeq: issueState.last_read_seq,
        titles: activityTitles,
        today,
      }),
    [activityTitles, events, issueState.last_read_seq, today]
  );
  const [showActivity, setShowActivity] = useShowActivity();
  const [showRetracted, setShowRetracted] = useShowRetracted();
  const [showResolvedComments, setShowResolvedComments] = useState(false);
  const retractedCount = useMemo(() => countRetractedAsks(items), [items]);
  const resolvedCommentCount = useMemo(
    () =>
      items.filter(
        (item) =>
          item.kind === "comment" &&
          (item.event.payload.resolved ||
            (item.event.payload.suggestion !== null &&
              item.event.payload.suggestion.accepted !== null))
      ).length,
    [items]
  );
  const shown = useMemo(
    () =>
      visibleConversationItems(items, { showActivity, showRetracted }).filter((item) => {
        if (item.kind !== "comment") {
          return true;
        }
        const focused =
          item.event.payload.id === focusItemId ||
          item.replies.some((reply) => reply.event.payload.id === focusItemId);
        const resolved =
          item.event.payload.resolved ||
          (item.event.payload.suggestion !== null &&
            item.event.payload.suggestion.accepted !== null);
        return showResolvedComments || !resolved || focused;
      }),
    [focusItemId, items, showActivity, showResolvedComments, showRetracted]
  );
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const [phoneThreadId, setPhoneThreadId] = useState<string>();
  const phoneThread = useMemo(
    () =>
      phoneThreadId === undefined || !isPhoneViewport
        ? undefined
        : shown.find(
            (item): item is Extract<ConversationItem, { kind: "comment" }> =>
              item.kind === "comment" && item.event.payload.id === phoneThreadId
          ),
    [isPhoneViewport, phoneThreadId, shown]
  );
  useEffect(() => {
    if (
      phoneThreadId !== undefined &&
      (!isPhoneViewport ||
        !shown.some((item) => item.kind === "comment" && item.event.payload.id === phoneThreadId))
    ) {
      setPhoneThreadId(undefined);
    }
  }, [isPhoneViewport, phoneThreadId, shown]);
  const phoneThreadDialog = useDialog<HTMLElement>({
    onClose: () => setPhoneThreadId(undefined),
    open: phoneThread !== undefined,
  });
  const commentActions = useCommentActionQueue({
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["events", issueKey] }),
  });
  const [ownSendCount, setOwnSendCount] = useState(0);
  const [replyTo, setReplyTo] = useState<ReplyTarget | null>(null);
  const phoneReplyTargetsThread =
    phoneThread !== undefined &&
    replyTo?.parentKind === "comment" &&
    replyTo.id === phoneThread.event.payload.id;
  const itemSeqs = useMemo(
    () => shown.flatMap((item) => ("lastSeq" in item ? [item.lastSeq] : [])),
    [shown]
  );
  // The focused turn may be a reply nested in a thread; its own `data-turn` is the anchor.
  const targetTurnId = useMemo(() => {
    if (focusItemId === undefined) return undefined;
    for (const item of shown) {
      if (item.kind === "ask" && item.ask.id === focusItemId) return item.id;
      if (item.kind === "comment") {
        if (item.event.payload.id === focusItemId) return item.id;
        const reply = item.replies.find((candidate) => candidate.event.payload.id === focusItemId);
        if (reply !== undefined) return reply.id;
      }
      if (item.kind === "message" || item.kind === "targeted-message") {
        if (item.event.payload.id === focusItemId) return item.id;
        const reply = item.replies.find((candidate) => candidate.event.payload.id === focusItemId);
        if (reply !== undefined) return reply.id;
      }
    }
    return undefined;
  }, [focusItemId, shown]);
  const follow = useFollowLatest({
    enabled: visible,
    itemSeqs,
    ownSendCount,
  });
  const { agents, titles } = useAgents(visible);
  const observedTurnKey = useMemo(
    () =>
      shown
        .filter((item) => "lastSeq" in item)
        .map((item) => `${item.id}:${item.lastSeq}`)
        .join(","),
    [shown]
  );
  const hasFailedOps = failedOps?.issueKey === issueKey;
  const lastFocusRequest = useRef<string | undefined>(undefined);
  const focusPageLoads = useRef(0);
  const scrolledToFocus = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (lastFocusRequest.current !== focusItemId) {
      lastFocusRequest.current = focusItemId;
      focusPageLoads.current = 0;
      scrolledToFocus.current = undefined;
    }
  }, [focusItemId]);

  useEffect(() => {
    if (!visible || focusItemId === undefined || scrolledToFocus.current === focusItemId) {
      return;
    }
    if (targetTurnId !== undefined) {
      const target = document.querySelector<HTMLElement>(`[data-turn="${targetTurnId}"]`);
      if (target !== null) {
        target.scrollIntoView({ block: "center" });
        scrolledToFocus.current = focusItemId;
      }
      return;
    }
    if (log.hasNextPage && !log.isFetchingNextPage && focusPageLoads.current < maxFocusPageLoads) {
      focusPageLoads.current += 1;
      void log.fetchNextPage();
    }
  }, [
    focusItemId,
    log.fetchNextPage,
    log.hasNextPage,
    log.isFetchingNextPage,
    targetTurnId,
    visible,
  ]);

  useEffect(() => {
    lastRead.current = issueState.last_read_seq;
  }, [issueState.last_read_seq]);

  useEffect(() => {
    if (!visible || observedTurnKey === "") {
      return;
    }
    const visibleEvents = new Set<Element>();
    let active = true;
    const scheduleRead = (target: Element, sequence: number) => {
      if (
        !active ||
        !visibleEvents.has(target) ||
        sequence <= lastRead.current ||
        timers.current.has(target)
      ) {
        return;
      }
      const timer = window.setTimeout(() => {
        timers.current.delete(target);
        if (!active || !visibleEvents.has(target) || sequence <= lastRead.current) {
          return;
        }
        lastRead.current = sequence;
        void api
          .putIssueState(issueKey, { last_read_seq: sequence })
          .then((next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [issueKey]: next,
            }));
          })
          .catch(async () => {
            const current = await queryClient.fetchQuery(userStateQuery()).catch(() => undefined);
            if (!active) {
              return;
            }
            lastRead.current = stateForIssue(current, issueKey).last_read_seq;
            scheduleRead(target, sequence);
          });
      }, 1_000);
      timers.current.set(target, timer);
    };
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const sequence = observed.current.get(entry.target);
          if (sequence === undefined) {
            continue;
          }
          if (!entry.isIntersecting) {
            visibleEvents.delete(entry.target);
            const currentTimer = timers.current.get(entry.target);
            if (currentTimer !== undefined) {
              window.clearTimeout(currentTimer);
              timers.current.delete(entry.target);
            }
            continue;
          }
          visibleEvents.add(entry.target);
          scheduleRead(entry.target, sequence);
        }
      },
      { threshold: 0.75 }
    );
    for (const [element] of observed.current) {
      if (!element.isConnected) {
        observed.current.delete(element);
        continue;
      }
      observed.current.set(element, Number(element.getAttribute("data-event-seq")));
      observer.observe(element);
    }
    return () => {
      active = false;
      observer.disconnect();
      for (const timer of timers.current.values()) {
        window.clearTimeout(timer);
      }
      timers.current.clear();
    };
  }, [issueKey, observedTurnKey, queryClient, visible]);

  const registerObserved = useCallback((element: HTMLElement | null) => {
    if (element !== null) {
      observed.current.set(element, Number(element.dataset.eventSeq));
    }
  }, []);

  const applyOptimisticOperations = (operations: PinStateOperation[]) => {
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      let next = stateForIssue(current, issueKey);
      for (const operation of operations) {
        next = {
          ...next,
          dismissed: applyPinStateOperation(next.dismissed, operation),
        };
      }
      return { ...current, [issueKey]: next };
    });
  };

  const enqueuePinOperations = (operations: PinStateOperation[]) => {
    for (const operation of operations) {
      void sharedIssueStateWrites
        .enqueue(issueKey, operation, {
          ...issueStateTransport,
          onDrained: (key, next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [key]: next,
            }));
            setFailedOps((current) => (current?.issueKey === key ? undefined : current));
            setRetryingFailedOps(false);
          },
          onError: (key, failedOperations, next) => {
            if (next === undefined) {
              void queryClient.invalidateQueries({ queryKey: ["user-state"] });
            } else {
              queryClient.setQueryData<UserState>(["user-state"], (current) => ({
                ...current,
                [key]: next,
              }));
            }
            setFailedOps({
              authoritativeState: next,
              issueKey: key,
              operations: failedOperations,
            });
            setRetryingFailedOps(false);
          },
        })
        .catch(() => {});
    }
  };

  const updatePins = (
    operation: PinStateOperation | ((dismissed: string[]) => PinStateOperation)
  ) => {
    if (hasFailedOps) {
      return;
    }
    let nextOperation: PinStateOperation | undefined;
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      const currentIssueState = stateForIssue(current, issueKey);
      nextOperation =
        typeof operation === "function" ? operation(currentIssueState.dismissed) : operation;
      return {
        ...current,
        [issueKey]: {
          ...currentIssueState,
          dismissed: applyPinStateOperation(currentIssueState.dismissed, nextOperation),
        },
      };
    });
    if (nextOperation !== undefined) {
      enqueuePinOperations([nextOperation]);
    }
  };

  const retryFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retryingFailedOps) {
      return;
    }
    setRetryingFailedOps(true);
    applyOptimisticOperations(failedOps.operations);
    enqueuePinOperations(failedOps.operations);
  };

  const dismissFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retryingFailedOps) {
      return;
    }
    const authoritativeState = failedOps.authoritativeState;
    if (authoritativeState === undefined) {
      void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    } else {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issueKey]: authoritativeState,
      }));
    }
    setFailedOps(undefined);
  };

  if (log.isPending) {
    return <p className={textMutedOnCanvas}>Loading conversation…</p>;
  }
  if (log.isError) {
    return <p className={dangerText}>Could not load the conversation.</p>;
  }

  // The reader's turn keeps its viewport position across live updates - a turn arriving while
  // they browse history, a pin reflow, the failed-operations banner, the unread divider moving -
  // except while they follow the latest turn, where an arrival simply appears in place.
  return (
    <ViewportAnchor
      as="section"
      className="flex min-h-[60dvh] flex-col gap-3 pb-56 sm:pb-16 xl:pb-0"
      enabled={() => !follow.pinnedToTop()}
      item="data-event-seq"
      label="Conversation"
    >
      {hasFailedOps ? (
        <div className={`flex items-center gap-3 text-sm ${dangerText}`} role="alert">
          <p>Couldn't save pin — retry</p>
          <button
            className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
            disabled={retryingFailedOps}
            onClick={retryFailedOperations}
            type="button"
          >
            Retry
          </button>
          <button
            className="underline disabled:cursor-not-allowed disabled:opacity-50"
            disabled={retryingFailedOps}
            onClick={dismissFailedOperations}
            type="button"
          >
            Close
          </button>
        </div>
      ) : null}
      {isClosed || (isPhoneViewport && phoneThreadId !== undefined) ? null : (
        <MentionComposer
          docked
          onCancelReply={() => setReplyTo(null)}
          onClose={() => setReplyTo(null)}
          onSent={() => {
            setReplyTo(null);
            setOwnSendCount((count) => count + 1);
          }}
          owner={{ issueKey, kind: "issue" }}
          replyTo={replyTo}
        />
      )}
      <div className="flex items-center justify-end gap-4">
        {retractedCount === 0 ? null : (
          <label className={`flex min-h-11 items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <input
              checked={showRetracted}
              className={`${checkboxAccent} min-h-11 min-w-11`}
              onChange={(event) => setShowRetracted(event.target.checked)}
              type="checkbox"
            />
            Show retracted ({retractedCount})
          </label>
        )}
        {resolvedCommentCount === 0 ? null : (
          <button
            aria-expanded={showResolvedComments}
            className={`min-h-11 text-sm font-medium ${textSecondaryOnCanvas}`}
            onClick={() => setShowResolvedComments((current) => !current)}
            type="button"
          >
            Resolved ({resolvedCommentCount})
          </button>
        )}
        <label className={`flex min-h-11 items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
          <input
            checked={showActivity}
            className={`${checkboxAccent} min-h-11 min-w-11`}
            onChange={(event) => setShowActivity(event.target.checked)}
            type="checkbox"
          />
          Show activity
        </label>
      </div>
      <ol
        aria-hidden={phoneThread === undefined ? undefined : true}
        aria-label="Conversation turns"
        className="flex flex-col gap-1"
        inert={phoneThread === undefined ? undefined : true}
      >
        {shown.map((item) => {
          if (item.kind === "day-divider" || item.kind === "unread-divider") {
            const label = item.kind === "day-divider" ? item.label : "New since you last read";
            return (
              // biome-ignore lint/a11y/useSemanticElements: A labeled list item is required by the Conversation DOM contract.
              // biome-ignore lint/a11y/useFocusableInteractive: A divider is descriptive, not an interactive control.
              <li
                aria-label={label}
                className={`my-2 flex items-center gap-3 text-xs font-semibold tracking-wide uppercase ${
                  item.kind === "unread-divider" ? linkText : textMutedOnCanvas
                }`}
                key={item.id}
                // biome-ignore lint/a11y: The role is required by the Conversation DOM contract.
                role="separator"
              >
                <span className={`h-px flex-1 ${newDividerLine}`} />
                {label}
                <span className={`h-px flex-1 ${newDividerLine}`} />
              </li>
            );
          }

          const pinned = isPinnedEvent(issueState.dismissed, item.pinEventId);
          const onPin = () =>
            updatePins((dismissed) => ({
              id: eventItemId({ id: item.pinEventId }),
              op: isPinnedEvent(dismissed, item.pinEventId) ? "unpin" : "pin",
            }));

          if (item.kind === "targeted-message") {
            return (
              <TargetedMessageTurn
                agents={agents}
                current={item.id === targetTurnId}
                currentTurnId={targetTurnId}
                isClosed={isClosed}
                issueKey={issueKey}
                item={item}
                key={item.id}
                onReply={setReplyTo}
                register={registerObserved}
                titles={titles}
              />
            );
          }
          if (item.kind === "message") {
            return (
              <MessageTurn
                agents={agents}
                author={resolveAuthor(item.author, titles)}
                issueKey={issueKey}
                current={item.id === targetTurnId}
                currentTurnId={targetTurnId}
                disabled={hasFailedOps}
                isClosed={isClosed}
                item={item}
                key={item.id}
                onPin={onPin}
                onReply={setReplyTo}
                pinned={pinned}
                register={registerObserved}
                titles={titles}
              />
            );
          }
          if (item.kind === "comment") {
            return (
              <CommentTurn
                actionError={commentActions.actionErrorId === item.event.payload.id}
                hideReplyComposer={
                  replyTo?.parentKind === "comment" && replyTo.id === item.event.payload.id
                }
                agents={agents}
                issueArtifacts={issueArtifacts}
                isPhone={isPhoneViewport}
                onPhoneThreadToggle={() => setPhoneThreadId(item.event.payload.id)}
                onAction={(id, kind) => commentActions.mutateItem({ id, kind })}
                current={item.id === targetTurnId}
                disabled={hasFailedOps}
                onRetryAction={commentActions.retryItem}
                pendingAction={commentActions.pendingActionIds.has(item.event.payload.id)}
                isClosed={isClosed}
                issueKey={issueKey}
                item={item}
                key={item.id}
                onPin={onPin}
                onReply={setReplyTo}
                pinned={pinned}
                register={registerObserved}
                viewerLogin={viewer.data?.login ?? ""}
              />
            );
          }
          if (item.kind === "ask") {
            return (
              <li
                aria-current={item.id === targetTurnId ? "true" : undefined}
                className="my-2 flex gap-3"
                data-event-seq={item.lastSeq}
                data-turn={item.id}
                key={item.id}
                ref={registerObserved}
              >
                <div className="min-w-0 flex-1">
                  <AskCard ask={item.ask} thread="collapsed" />
                </div>
                <TurnPin disabled={hasFailedOps} onPin={onPin} pinned={pinned} />
              </li>
            );
          }
          return (
            <li
              className={`flex items-baseline gap-2 px-2 text-xs ${textMutedOnCanvas}`}
              data-event-seq={item.lastSeq}
              data-kind="activity"
              data-turn={item.id}
              key={item.id}
              ref={registerObserved}
            >
              <span className="font-medium">{resolveAuthor(item.author, titles).label}</span>
              <ActivityLine description={item.description} event={item.event} issueKey={issueKey} />
              <span aria-hidden="true">·</span>
              <Timestamp at={item.at} />
            </li>
          );
        })}
      </ol>
      {phoneThread === undefined ? null : (
        <section
          aria-label="Thread"
          aria-modal="true"
          className={`fixed inset-0 z-20 flex flex-col ${card}`}
          ref={phoneThreadDialog.containerRef}
          role="dialog"
        >
          <header className={`flex items-center border-b px-4 py-3 ${borderDefault}`}>
            <button
              className={`min-h-11 text-sm font-medium ${textPrimaryOnSurface}`}
              onClick={() => setPhoneThreadId(undefined)}
              type="button"
            >
              Back
            </button>
          </header>
          <ol className="min-h-0 flex-1 overflow-y-auto px-4 pb-32">
            <CommentTurn
              actionError={commentActions.actionErrorId === phoneThread.event.payload.id}
              agents={agents}
              issueArtifacts={issueArtifacts}
              composerClassName={`fixed inset-x-0 bottom-0 z-20 border-t px-4 pt-4 pb-2 ${card} ${borderDefault}`}
              current={phoneThread.id === targetTurnId}
              viewerLogin={viewer.data?.login ?? ""}
              disabled={hasFailedOps}
              forceExpanded
              hideReplyComposer={phoneReplyTargetsThread}
              isClosed={isClosed}
              issueKey={issueKey}
              item={phoneThread}
              isPhone
              key={phoneThread.id}
              onAction={(id, kind) => commentActions.mutateItem({ id, kind })}
              onPhoneThreadToggle={() => setPhoneThreadId(undefined)}
              onRetryAction={commentActions.retryItem}
              pendingAction={commentActions.pendingActionIds.has(phoneThread.event.payload.id)}
              onPin={() =>
                updatePins((dismissed) => ({
                  id: eventItemId({ id: phoneThread.pinEventId }),
                  op: isPinnedEvent(dismissed, phoneThread.pinEventId) ? "unpin" : "pin",
                }))
              }
              onReply={setReplyTo}
              pinned={isPinnedEvent(issueState.dismissed, phoneThread.pinEventId)}
            />
          </ol>
          {phoneReplyTargetsThread ? (
            <div
              className={`fixed inset-x-0 bottom-0 z-20 border-t px-4 pt-4 pb-2 ${card} ${borderDefault}`}
            >
              <MentionComposer
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
          ) : null}
        </section>
      )}
      {shown.length === 0 ? <p className={textMutedOnCanvas}>No messages yet.</p> : null}
      {log.hasNextPage ? (
        <div className="flex items-center justify-center">
          <button
            className={`min-h-11 rounded-lg px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={log.isFetchingNextPage}
            onClick={() => void log.fetchNextPage()}
            type="button"
          >
            {log.isFetchingNextPage ? "Loading…" : "Load older"}
          </button>
        </div>
      ) : null}
      {visible && !follow.atTop && shown.length > 0 ? (
        <button
          className={`fixed right-6 bottom-36 z-20 min-h-11 rounded-full px-4 text-sm font-semibold shadow-lg xl:bottom-24 ${primaryButtonBg} ${primaryButtonHoverBg}`}
          data-testid="jump-to-latest"
          onClick={follow.jumpToLatest}
          type="button"
        >
          Jump to latest{follow.newItemCount === 0 ? "" : ` · ${follow.newItemCount} new`}
        </button>
      ) : null}
    </ViewportAnchor>
  );
}
